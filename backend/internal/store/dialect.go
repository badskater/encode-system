package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	// pgx stdlib — registers the "pgx" driver for database/sql. Imported for
	// its side effect (driver registration) so Open() can sql.Open("pgx", dsn).
	_ "github.com/jackc/pgx/v5/stdlib"
)

// isPostgres reports whether this store is backed by Postgres (pgx stdlib).
// SQLite is the default (false). Set once in Open().
func (s *Store) isPostgres() bool { return s.dialect == dialectPostgres }

// dialect identifies the SQL backend so queries can be rewritten to match.
type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// pgNowExpr is the Postgres equivalent of SQLite's datetime('now'): a TEXT
// value in "YYYY-MM-DD HH:MM:SS" UTC, matching the column format exactly so
// parseTime (which parses "2006-01-02 15:04:05") reads it unchanged.
const pgNowExpr = "to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')"

// q rebinds ? placeholders to $N for Postgres and translates SQLite-specific
// SQL functions to their Postgres equivalents. On SQLite it is a no-op — the
// SQL string is returned verbatim so SQLite behavior is byte-for-byte
// unchanged.
//
// Every query in the store package routes through q() so the dialect split
// lives in one place. The rebind skips single-quoted string literals so a '?'
// that is data (not a placeholder) is never rewritten — e.g. WHERE x = 'a?b'
// keeps its literal '?' intact while WHERE y = ? becomes WHERE y = $1.
//
// On Postgres, q() also translates datetime('now') to the PG now expression.
// The heavier SQLite functions (julianday, strftime, date, SUBSTR,
// json_extract, substr) are NOT translated by q(); instead, migratePG creates
// PL/pgSQL compatibility functions with the same names so the identical query
// text runs on both backends. This keeps the stats query construction
// dialect-agnostic (no per-dialect SQL strings) while avoiding fragile
// in-string function rewriting.
func (s *Store) q(sqlText string) string {
	if s.dialect != dialectPostgres {
		return sqlText // SQLite: native ? placeholders, native functions
	}
	return rebindPostgres(translateDatetimeNow(sqlText))
}

// translateDatetimeNow rewrites SQLite's datetime('now') to the Postgres
// equivalent. Only the no-arg form (datetime('now')) is translated here;
// the two-arg modifier form (datetime('now', ?)) is handled directly at
// the call site (PruneNodeMetrics) with a dialect branch, since the PG
// equivalent requires an interval cast and a format string that simple
// string replacement can't inject reliably.
func translateDatetimeNow(sql string) string {
	// datetime('now') → PG now expression.
	sql = strings.ReplaceAll(sql, "datetime('now')", pgNowExpr)
	return sql
}

// rebindPostgres replaces ? with $1, $2, … skipping single-quoted literals.
// It is the standalone (testable) form of the placeholder rebind; Store.q
// calls translateSQLiteFuncs first, then delegates here. The quote state
// machine handles SQLite/PG SQL string-literal rules:
//   - ” inside a literal is an escaped quote (stays in literal state)
//   - a lone ' toggles in/out of the literal
//
// A '?' inside a literal is data and is never rebound. A '?' outside a
// literal is a bind parameter and is rewritten to $N.
func rebindPostgres(sqlText string) string {
	var b strings.Builder
	b.Grow(len(sqlText) + 16) // headroom for the $N suffixes
	inQuote := false
	paramIdx := 0
	for i := 0; i < len(sqlText); i++ {
		ch := sqlText[i]
		if ch == '\'' {
			// Check for escaped '' (two single quotes in a row) — stay in
			// the same quote state so the literal isn't prematurely closed.
			if inQuote && i+1 < len(sqlText) && sqlText[i+1] == '\'' {
				b.WriteByte(ch)
				b.WriteByte(sqlText[i+1])
				i++ // skip the second quote
				continue
			}
			inQuote = !inQuote
			b.WriteByte(ch)
			continue
		}
		if ch == '?' && !inQuote {
			paramIdx++
			fmt.Fprintf(&b, "$%d", paramIdx)
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// insertID executes an INSERT and returns the new row's id. On SQLite it uses
// Result.LastInsertId (the driver-supported path). On Postgres it appends
// " RETURNING id" and scans the single returned id — pgx does not support
// LastInsertId, and RETURNING is the portable Postgres idiom.
//
// The query passed in must NOT already contain a RETURNING clause. Call
// sites that today do `res, err := Exec(...); id, err := res.LastInsertId()`
// route through this helper so both backends resolve the id correctly.
func (s *Store) insertID(ctx context.Context, query string, args ...any) (int64, error) {
	if s.dialect != dialectPostgres {
		res, err := s.db.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	// Postgres: append RETURNING id and scan the single row.
	q := s.q(query + " RETURNING id")
	var id int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// nowExpr returns the SQL expression that produces the current timestamp as
// a TEXT value in "YYYY-MM-DD HH:MM:SS" UTC — the format fmtTime/parseTime
// and every timestamp column use. SQLite: datetime('now'). Postgres:
// to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS'). The PG default
// matches SQLite's datetime('now') byte-for-byte so all scan code (which
// parses TEXT timestamps via parseTime) is unchanged. Call sites that build
// SQL dynamically (not through q()) use this for dialect-correct timestamps.
func (s *Store) nowExpr() string {
	if s.dialect == dialectPostgres {
		return pgNowExpr
	}
	return "datetime('now')"
}

// qRow is a thin wrapper that rebinds a query for the current dialect and
// delegates to db.QueryRowContext. It exists so call sites read clearly as
// "dialect-aware query" rather than a bare s.q(...) + QueryRowContext pair.
func (s *Store) qRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.q(query), args...)
}

// qExec rebinds and executes. Same readability rationale as qRow.
func (s *Store) qExec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.q(query), args...)
}

// qQuery rebinds and queries. Same readability rationale as qRow.
func (s *Store) qQuery(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.q(query), args...)
}

// forUpdate appends " FOR UPDATE" on Postgres and returns the SQL unchanged
// on SQLite. Used inside a transaction to take a row lock that serializes
// concurrent access to the same row across replicas — e.g. the per-node
// capacity check in AssignJob SELECTs the node row FOR UPDATE so two
// controllers dispatching to the same node cannot both pass the capacity
// count and overshoot max_concurrent_jobs.
//
// FOR UPDATE is a syntax error on SQLite, and SQLite's single-writer
// (MaxOpenConns=1) model already serializes every transaction, so the lock
// is unnecessary there — the helper is a no-op that keeps call sites
// identical across backends.
//
// Only valid inside a transaction (BEGIN … COMMIT); outside a tx the lock
// is released immediately and serializes nothing.
func (s *Store) forUpdate(sqlText string) string {
	if s.dialect != dialectPostgres {
		return sqlText
	}
	return sqlText + " FOR UPDATE"
}
