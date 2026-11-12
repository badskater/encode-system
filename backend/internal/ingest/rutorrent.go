package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// RuTorrent is a BEST-EFFORT, EXPERIMENTAL client for the ruTorrent
// httprpc plugin (plugins/httprpc/action.php). The plugin speaks
// form-encoded RPC modes that map to rtorrent XMLRPC multicalls and
// answer JSON:
//
//   - mode=list (plus cmd=d.get_complete=) → d.multicall over the main
//     view; the response {"t": {hash: [values…]}} carries the fixed
//     35-column layout below, with the appended complete flag last.
//   - mode=fls&hash=H → f.multicall file rows
//     [path, completed_chunks, size_chunks, size_bytes, priority].
//
// The category filter matches the ruTorrent LABEL (rtorrent d.custom1).
// Auth is whatever the web server in front of ruTorrent enforces; this
// client supports HTTP Basic (username+password). Digest-only setups are
// NOT supported — the feature is experimental and qBittorrent is the
// first-class provider.
type RuTorrent struct {
	base     string // ruTorrent web root, no trailing slash
	username string
	password string
	http     *http.Client
	log      *slog.Logger
}

// NewRuTorrent builds a client for the ruTorrent web root at baseURL
// (e.g. http://host/rutorrent).
func NewRuTorrent(baseURL, username, password string, log *slog.Logger) (*RuTorrent, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("rutorrent: invalid URL %q", baseURL)
	}
	if log == nil {
		log = slog.Default()
	}
	return &RuTorrent{
		base:     base,
		username: username,
		password: password,
		http:     newBoundedClient(nil),
		log:      log,
	}, nil
}

// Column indices into the mode=list per-torrent value array. The plugin
// keys each torrent by its hash and returns the REMAINING 35 d.multicall
// columns in this fixed order (plugins/httprpc/action.php, mode "list").
// The appended cmd=d.get_complete= value lands at index 35 when present.
const (
	rtColState       = 3  // d.get_state
	rtColName        = 4  // d.get_name
	rtColBytesDone   = 8  // d.get_bytes_done
	rtColCustom1     = 14 // d.get_custom1 (the ruTorrent label)
	rtColLeftBytes   = 19 // d.get_left_bytes
	rtColBasePath    = 25 // d.get_base_path
	rtColIsMultiFile = 33 // d.is_multi_file
	rtColComplete    = 35 // appended d.get_complete (0/1)
	rtMinColumns     = 35 // fixed layout length without the append
)

// rpc POSTs one form-encoded mode request to the httprpc endpoint and
// returns the raw JSON body. Every exchange is bounded (client timeout +
// caller ctx); HTTP Basic is attached when a username is configured.
func (r *RuTorrent) rpc(ctx context.Context, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.base+"/plugins/httprpc/action.php", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if r.username != "" {
		req.SetBasicAuth(r.username, r.password)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rutorrent rpc: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("rutorrent rpc: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rutorrent rpc: HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// rtListResponse is the mode=list JSON shape: torrents keyed by hash.
// Values arrive as strings OR numbers depending on the PHP JSON encoder,
// so each cell is decoded as any and stringified at use.
type rtListResponse struct {
	T map[string][]any `json:"t"`
}

// Completed lists completed torrents via mode=list with the appended
// d.get_complete command, filters by label (category) client-side, and
// fetches each candidate's file list via mode=fls. Per-torrent failures
// are logged and skipped (retried next poll), never fatal to the batch.
func (r *RuTorrent) Completed(ctx context.Context, category string) ([]Torrent, error) {
	form := url.Values{}
	form.Set("mode", "list")
	// Append the authoritative completion flag to the fixed column set.
	form.Add("cmd", "d.get_complete=")
	body, err := r.rpc(ctx, form)
	if err != nil {
		return nil, err
	}
	var list rtListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("rutorrent: decode torrent list: %w", err)
	}
	var out []Torrent
	for hash, vals := range list.T {
		if hash == "" || len(vals) < rtMinColumns {
			continue // malformed/short row: skip, never guess
		}
		if !rtComplete(vals) {
			continue
		}
		label := rtString(vals[rtColCustom1])
		if category != "" && label != category {
			continue
		}
		basePath := rtString(vals[rtColBasePath])
		if basePath == "" {
			continue
		}
		// d.get_base_path is the torrent's top-level file (single-file)
		// or directory (multi-file). The save directory is the parent
		// for single-file torrents.
		savePath := basePath
		if rtString(vals[rtColIsMultiFile]) != "1" {
			savePath = path.Dir(basePath)
		}
		files, err := r.files(ctx, hash)
		if err != nil {
			r.log.Warn("ingest: torrent files fetch failed; skipping this poll",
				"provider", model.IngestProviderRuTorrent, "hash", hash, "err", err)
			continue
		}
		out = append(out, Torrent{
			Hash:     hash,
			Name:     rtString(vals[rtColName]),
			Category: label,
			SavePath: savePath,
			Files:    files,
		})
	}
	return out, nil
}

// rtComplete reports whether a torrent row is complete. The appended
// d.get_complete flag is authoritative when present; on a plugin build
// that drops the appended command we fall back to left_bytes==0 (fully
// downloaded) — best-effort, documented in the package comment.
func rtComplete(vals []any) bool {
	if len(vals) > rtColComplete {
		return rtString(vals[rtColComplete]) == "1"
	}
	return rtString(vals[rtColLeftBytes]) == "0"
}

// files fetches one torrent's file rows (mode=fls). Row layout:
// [path, completed_chunks, size_chunks, size_bytes, priority].
func (r *RuTorrent) files(ctx context.Context, hash string) ([]File, error) {
	form := url.Values{}
	form.Set("mode", "fls")
	form.Set("hash", hash)
	body, err := r.rpc(ctx, form)
	if err != nil {
		return nil, err
	}
	var rows [][]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("rutorrent: decode file list: %w", err)
	}
	var out []File
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		name := rtString(row[0])
		if name == "" {
			continue
		}
		out = append(out, File{Name: name, Size: rtInt(row[3])})
	}
	return out, nil
}

// rtString normalizes one JSON cell (string or number) to its string form.
func rtString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// rtInt normalizes one JSON cell to an int64 (sizes arrive as numbers or
// numeric strings). A non-numeric cell degrades to 0 — size is advisory
// for staging (the copy reads the real file).
func rtInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		var n int64
		fmt.Sscanf(t, "%d", &n)
		return n
	default:
		return 0
	}
}
