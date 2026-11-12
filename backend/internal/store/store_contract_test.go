package store

// This file holds the dual-backend contract test suite. storeContract(t, openFn)
// runs identical assertions against whatever backend openFn produces. The suite
// is invoked twice:
//   - SQLite: always (openFn = tmpfile SQLite Open) — this is the default
//     `go test ./...` path, no env required.
//   - Postgres: only when ENCODE_TEST_PG_DSN is set. The PG run uses the
//     encode_test database and TRUNCATEs all tables in t.Cleanup so each test
//     starts from an empty database. This proves migratePG creates everything
//     from scratch on a fresh DB.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// storeContract runs the core behavior contract against the store produced by
// openFn. Both backends must pass this identical suite.
func storeContract(t *testing.T, openFn func(t *testing.T) *Store) {
	ctx := context.Background()
	s := openFn(t)

	// --- Node CRUD + platform default ---
	n, err := s.CreateNode(ctx, "contract-node", "hash-contract")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if n.Name != "contract-node" || n.TokenHash != "hash-contract" {
		t.Fatalf("node round-trip: %+v", n)
	}
	if !n.Enabled {
		t.Fatal("new node should default to enabled")
	}
	if n.Platform != "windows" {
		t.Fatalf("platform default = %q, want windows", n.Platform)
	}
	got, err := s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.Name != n.Name {
		t.Fatalf("get node name mismatch: %s", got.Name)
	}
	n.Status = model.NodeBusy
	n.Platform = "linux"
	if err := s.UpdateNode(ctx, n); err != nil {
		t.Fatalf("update node: %v", err)
	}
	got, err = s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node after update: %v", err)
	}
	if got.Status != model.NodeBusy || got.Platform != "linux" {
		t.Fatalf("update not persisted: %+v", got)
	}

	// --- Flow CRUD ---
	flow, err := s.CreateFlow(ctx, &model.Flow{
		Name:  "contract-flow",
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatalf("create flow: %v", err)
	}
	if flow.ID == 0 {
		t.Fatal("flow has no id")
	}
	gotFlow, err := s.GetFlow(ctx, flow.ID)
	if err != nil {
		t.Fatalf("get flow: %v", err)
	}
	if gotFlow.Name != "contract-flow" || len(gotFlow.Steps) != 1 {
		t.Fatalf("flow round-trip: %+v", gotFlow)
	}

	// --- Job create + status transitions + step timings ---
	node, err := s.CreateNode(ctx, "contract-job-node", "jh")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJob(ctx, &model.Job{
		Series: "ContractShow", Episode: "01", EpisodeDir: "ContractShow/Ep 01",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.Status != model.JobPending {
		t.Fatalf("new job status = %s, want pending", job.Status)
	}
	if err := s.AssignJob(ctx, job.ID, node.ID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := s.UpdateJobStatus(ctx, job.ID, model.JobRunning, "encode", 42.5, "tail"); err != nil {
		t.Fatalf("update status: %v", err)
	}
	gotJob, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if gotJob.Status != model.JobRunning || gotJob.Step != "encode" || gotJob.Progress != 42.5 {
		t.Fatalf("status update not persisted: %+v", gotJob)
	}
	// Finish with report (step timings + metrics).
	timings := []model.StepTiming{{Step: "encode", DurationSec: 120.5}}
	metrics := map[string]float64{"duration_sec": 300, "fps": 25}
	if err := s.FinishJobWithReport(ctx, job.ID, model.JobDone, 0, "", []string{"out.mkv"}, "tail", "fulllog", timings, metrics); err != nil {
		t.Fatalf("finish with report: %v", err)
	}
	gotJob, err = s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job after finish: %v", err)
	}
	if gotJob.Status != model.JobDone {
		t.Fatalf("status = %s, want done", gotJob.Status)
	}
	if len(gotJob.StepTimings) != 1 || gotJob.StepTimings[0].Step != "encode" {
		t.Fatalf("step timings not persisted: %+v", gotJob.StepTimings)
	}
	if gotJob.Metrics["duration_sec"] != 300 {
		t.Fatalf("metrics not persisted: %+v", gotJob.Metrics)
	}
	if len(gotJob.Outputs) != 1 || gotJob.Outputs[0] != "out.mkv" {
		t.Fatalf("outputs not persisted: %+v", gotJob.Outputs)
	}

	// --- Settings round-trip (JSON blob incl. linux_* fields) ---
	st := &model.Settings{
		ControllerURL:       "http://ctrl:8080",
		NFSServer:           "nas01",
		ScriptsShare:        "/scripts",
		ReleaseShare:        "/release",
		LinuxBinDir:         "/opt/encode/bin",
		LinuxScriptsDir:     "/opt/encode/scripts",
		LinuxReleaseDir:     "/opt/encode/release",
		DiscordWebhook:      "https://discord.example/x",
		ScanIntervalSeconds: 30,
		TasksBeforeReboot:   10,
	}
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	gotSt, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if gotSt == nil {
		t.Fatal("settings not found")
	}
	if gotSt.ControllerURL != "http://ctrl:8080" {
		t.Fatalf("settings controller_url = %q", gotSt.ControllerURL)
	}
	if gotSt.LinuxBinDir != "/opt/encode/bin" {
		t.Fatalf("settings linux_bin_dir = %q", gotSt.LinuxBinDir)
	}
	if gotSt.LinuxScriptsDir != "/opt/encode/scripts" {
		t.Fatalf("settings linux_scripts_dir = %q", gotSt.LinuxScriptsDir)
	}

	// --- Audit append/read ---
	if err := s.AppendAudit(ctx, "admin", "create", "node/contract", "created contract-node"); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	events, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Actor != "admin" || events[0].Action != "create" {
		t.Fatalf("audit event mismatch: %+v", events)
	}

	// --- Stats: CountJobsByStatus ---
	counts, err := s.CountJobsByStatus(ctx)
	if err != nil {
		t.Fatalf("count jobs by status: %v", err)
	}
	if counts["done"] < 1 {
		t.Fatalf("counts[done] = %d, want >= 1", counts["done"])
	}

	// --- Stats: JobStats aggregate (per-node/per-flow GROUP BY joins) ---
	// Regression guard: Postgres rejects non-grouped selected columns
	// (n.name/f.name must appear in GROUP BY) where SQLite tolerates them,
	// so this exercises the aggregate joins on both backends.
	stats7, err := s.JobStats(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("JobStats(7d): %v", err)
	}
	if stats7.Totals.Done < 1 {
		t.Fatalf("JobStats(7d) totals.done = %d, want >= 1", stats7.Totals.Done)
	}
	if len(stats7.PerNode) < 1 || stats7.PerNode[0].Name != "contract-job-node" {
		t.Fatalf("JobStats(7d) per-node mismatch: %+v", stats7.PerNode)
	}
	if len(stats7.PerFlow) < 1 || stats7.PerFlow[0].Name != flow.Name {
		t.Fatalf("JobStats(7d) per-flow mismatch: %+v", stats7.PerFlow)
	}
	statsAll, err := s.JobStats(ctx, 0)
	if err != nil {
		t.Fatalf("JobStats(all): %v", err)
	}
	if statsAll.Totals.Done < 1 {
		t.Fatalf("JobStats(all) totals.done = %d, want >= 1", statsAll.Totals.Done)
	}

	// --- Share CRUD with encrypted password ---
	sh, err := s.CreateShare(ctx, &model.Share{
		Name: "contract-share", Kind: model.ShareSMB, Role: model.ShareRoleScripts,
		Server: "smb-host", Path: "share", Username: "u", Password: "s3cret",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	gotShare, err := s.GetShare(ctx, sh.ID)
	if err != nil {
		t.Fatalf("get share: %v", err)
	}
	if gotShare.Password != "s3cret" {
		t.Fatalf("share password decrypt = %q, want s3cret", gotShare.Password)
	}

	// --- Metrics ring ---
	if err := s.InsertNodeMetric(ctx, node.ID, &model.NodeMetrics{
		CPUPct: 55.0, MemUsedMB: 1024, MemTotalMB: 4096, DiskFreeGB: 100,
	}); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	latest, err := s.LatestNodeMetric(ctx, node.ID)
	if err != nil {
		t.Fatalf("latest metric: %v", err)
	}
	if latest == nil || latest.CPUPct != 55.0 {
		t.Fatalf("latest metric = %+v", latest)
	}

	// --- Retention/prune ---
	// Seed a done job finished 30 days ago so the retention prune has a target.
	// PruneOldJobs(days) deletes terminal jobs finished more than `days` ago;
	// a just-created job won't match, so we backdate via SeedFinishedJob.
	oldTime := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	if err := s.SeedFinishedJob(ctx, flow.ID, node.ID, "done", "encode", oldTime, oldTime); err != nil {
		t.Fatalf("seed finished job: %v", err)
	}
	pruned, err := s.PruneOldJobs(ctx, 7) // prune jobs older than 7 days
	if err != nil {
		t.Fatalf("prune old jobs: %v", err)
	}
	if pruned < 1 {
		t.Fatalf("pruned = %d, want >= 1 (the backdated done job)", pruned)
	}

	// --- Retry/steering (last_failed_node_id) ---
	retryJob, err := s.CreateJob(ctx, &model.Job{
		Series: "RetryShow", Episode: "01", EpisodeDir: "RetryShow/Ep 01",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create retry job: %v", err)
	}
	if err := s.AssignJob(ctx, retryJob.ID, node.ID); err != nil {
		t.Fatalf("assign retry job: %v", err)
	}
	if err := s.FinishJob(ctx, retryJob.ID, model.JobFailed, 1, "crash", nil, "tail"); err != nil {
		t.Fatalf("finish retry job: %v", err)
	}
	gotRetry, _ := s.GetJob(ctx, retryJob.ID)
	if gotRetry.LastFailedNodeID != node.ID {
		t.Fatalf("last_failed_node_id = %d, want %d", gotRetry.LastFailedNodeID, node.ID)
	}
	if n, err := s.RetryJob(ctx, retryJob.ID); err != nil || n != 1 {
		t.Fatalf("retry: n=%d err=%v", n, err)
	}
	gotRetry, _ = s.GetJob(ctx, retryJob.ID)
	if gotRetry.Status != model.JobPending {
		t.Fatalf("retry didn't re-queue: %+v", gotRetry)
	}
	// last_failed_node_id survives retry — it's only cleared by a successful
	// finish (done), which lets the dispatcher steer the retry away from the
	// node that failed. A done finish clears it:
	if err := s.AssignJob(ctx, retryJob.ID, node.ID); err != nil {
		t.Fatalf("assign retried job: %v", err)
	}
	if err := s.FinishJob(ctx, retryJob.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatalf("finish retried job done: %v", err)
	}
	gotRetry, _ = s.GetJob(ctx, retryJob.ID)
	if gotRetry.LastFailedNodeID != 0 {
		t.Fatalf("done finish should clear steering: last_failed_node_id = %d", gotRetry.LastFailedNodeID)
	}

	// --- Flow options: job timeout + per-step timeout round-trip ---
	toFlow, err := s.CreateFlow(ctx, &model.Flow{
		Name:              "contract-timeout-flow",
		Steps:             []model.Step{{Type: model.StepEncode, TimeoutMinutes: 45}, {Type: model.StepMux}},
		MaxRetries:        1,
		JobTimeoutMinutes: 240,
	})
	if err != nil {
		t.Fatalf("create timeout flow: %v", err)
	}
	gotTO, err := s.GetFlow(ctx, toFlow.ID)
	if err != nil {
		t.Fatalf("get timeout flow: %v", err)
	}
	if gotTO.JobTimeoutMinutes != 240 || gotTO.MaxRetries != 1 {
		t.Fatalf("flow options round-trip: %+v", gotTO)
	}
	if len(gotTO.Steps) != 2 || gotTO.Steps[0].TimeoutMinutes != 45 || gotTO.Steps[1].TimeoutMinutes != 0 {
		t.Fatalf("step timeout round-trip: %+v", gotTO.Steps)
	}
	// PUT-style update replaces the options (timeout OFF must persist).
	gotTO.JobTimeoutMinutes = 0
	gotTO.Steps[0].TimeoutMinutes = 0
	if err := s.UpdateFlow(ctx, gotTO); err != nil {
		t.Fatalf("update timeout flow: %v", err)
	}
	gotTO, _ = s.GetFlow(ctx, toFlow.ID)
	if gotTO.JobTimeoutMinutes != 0 || gotTO.Steps[0].TimeoutMinutes != 0 {
		t.Fatalf("timeout OFF did not persist: %+v", gotTO)
	}

	// --- Stall watchdog: last_progress_at claim stamp + touch + query ---
	wdNode, err := s.CreateNode(ctx, "contract-wd-node", "wdh")
	if err != nil {
		t.Fatal(err)
	}
	wdJob, err := s.CreateJob(ctx, &model.Job{
		Series: "WatchShow", Episode: "01", EpisodeDir: "WatchShow/Ep 01",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create watchdog job: %v", err)
	}
	// Pending jobs carry no stamp and are never returned by the query.
	if ts, err := s.JobLastProgressAt(ctx, wdJob.ID); err != nil || ts != nil {
		t.Fatalf("pending job stamp = %v err=%v, want nil", ts, err)
	}
	if err := s.AssignJob(ctx, wdJob.ID, wdNode.ID); err != nil {
		t.Fatalf("assign watchdog job: %v", err)
	}
	claimed, err := s.JobLastProgressAt(ctx, wdJob.ID)
	if err != nil || claimed == nil {
		t.Fatalf("claim must stamp last_progress_at: %v err=%v", claimed, err)
	}
	if err := s.UpdateJobStatus(ctx, wdJob.ID, model.JobRunning, "encode", 10, ""); err != nil {
		t.Fatalf("run watchdog job: %v", err)
	}
	// Backdate the stamp (second-granularity timestamps make a sleep-based
	// assertion flaky; the contract suite controls time explicitly).
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := s.SeedJobLastProgress(ctx, wdJob.ID, old); err != nil {
		t.Fatalf("backdate progress: %v", err)
	}
	stalled, err := s.StalledRunningJobs(ctx, time.Now().UTC().Add(-1*time.Hour))
	if err != nil {
		t.Fatalf("stalled query: %v", err)
	}
	if len(stalled) != 1 || stalled[0].ID != wdJob.ID {
		t.Fatalf("stalled jobs = %+v, want exactly the backdated running job", stalled)
	}
	// TouchJobProgress refreshes the stamp → the job leaves the stalled set.
	if err := s.TouchJobProgress(ctx, wdJob.ID); err != nil {
		t.Fatalf("touch progress: %v", err)
	}
	refreshed, err := s.JobLastProgressAt(ctx, wdJob.ID)
	if err != nil || refreshed == nil || !refreshed.After(old) {
		t.Fatalf("touch did not refresh stamp: %v (was %v) err=%v", refreshed, old, err)
	}
	stalled, err = s.StalledRunningJobs(ctx, time.Now().UTC().Add(-1*time.Hour))
	if err != nil || len(stalled) != 0 {
		t.Fatalf("fresh-progress job must not stall: %+v err=%v", stalled, err)
	}
	// A terminal job is never returned even when its stamp is ancient.
	if err := s.FinishJob(ctx, wdJob.ID, model.JobFailed, 1, "stalled: no progress for 60 minutes", nil, ""); err != nil {
		t.Fatalf("finish watchdog job: %v", err)
	}
	if err := s.SeedJobLastProgress(ctx, wdJob.ID, old); err != nil {
		t.Fatal(err)
	}
	stalled, err = s.StalledRunningJobs(ctx, time.Now().UTC())
	if err != nil || len(stalled) != 0 {
		t.Fatalf("terminal job must not stall: %+v err=%v", stalled, err)
	}
	// The stall counter derives from failed rows with the stall prefix.
	if n, err := s.CountStalledJobs(ctx); err != nil || n != 1 {
		t.Fatalf("CountStalledJobs = %d err=%v, want 1", n, err)
	}

	// --- Series delete: registry row only, job history preserved ---
	delSr, err := s.UpsertSeriesByName(ctx, "ContractDeleteShow")
	if err != nil {
		t.Fatalf("upsert delete-show: %v", err)
	}
	delJob, err := s.CreateJob(ctx, &model.Job{
		Series: "ContractDeleteShow", Episode: "01", EpisodeDir: "ContractDeleteShow/Ep 01",
		ScriptType: "vpy", FlowID: flow.ID,
	})
	if err != nil {
		t.Fatalf("create delete-show job: %v", err)
	}
	if err := s.FinishJob(ctx, delJob.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatalf("finish delete-show job: %v", err)
	}
	progressBefore, err := s.SeriesProgressCounts(ctx)
	if err != nil {
		t.Fatalf("progress before delete: %v", err)
	}
	if err := s.DeleteSeries(ctx, delSr.ID); err != nil {
		t.Fatalf("delete series: %v", err)
	}
	if _, err := s.GetSeries(ctx, delSr.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetSeries after delete: err=%v, want sql.ErrNoRows", err)
	}
	// Deleting an unknown id is a not-found error, not a silent success.
	if err := s.DeleteSeries(ctx, delSr.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("repeat DeleteSeries: err=%v, want sql.ErrNoRows", err)
	}
	// The job survives with its denormalized series name and status.
	gotDelJob, err := s.GetJob(ctx, delJob.ID)
	if err != nil {
		t.Fatalf("job lost after series delete: %v", err)
	}
	if gotDelJob.Series != "ContractDeleteShow" || gotDelJob.Status != model.JobDone {
		t.Fatalf("job mutated after series delete: %+v", gotDelJob)
	}
	exists, err := s.JobExistsForEpisode(ctx, "ContractDeleteShow/Ep 01")
	if err != nil || !exists {
		t.Fatalf("episode dedupe key lost: exists=%v err=%v", exists, err)
	}
	// Progress counts still derive from the surviving job rows.
	progressAfter, err := s.SeriesProgressCounts(ctx)
	if err != nil {
		t.Fatalf("progress after delete: %v", err)
	}
	if progressBefore["ContractDeleteShow"] != progressAfter["ContractDeleteShow"] {
		t.Fatalf("progress changed by series delete: %+v → %+v",
			progressBefore["ContractDeleteShow"], progressAfter["ContractDeleteShow"])
	}
	// Re-upsert (what the scanner does on the next scan) creates a FRESH row.
	reSr, err := s.UpsertSeriesByName(ctx, "ContractDeleteShow")
	if err != nil {
		t.Fatalf("re-upsert after delete: %v", err)
	}
	if reSr.ID == delSr.ID {
		t.Fatalf("re-upsert reused deleted id %d", delSr.ID)
	}
	if !reSr.Enabled {
		t.Fatalf("re-upserted series must be enabled: %+v", reSr)
	}

	// --- Users: provision, role sync, sessions (OIDC P10) ---
	// CreateUser/UserByUsername/CreateSession were previously only exercised
	// through the API package (SQLite); the contract suite now pins them on
	// BOTH backends together with UpdateUserRole (the OIDC role-sync path).
	u, err := s.CreateUser(ctx, "contract-user", "", model.RoleViewer)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.Username != "contract-user" || u.Role != model.RoleViewer {
		t.Fatalf("user round-trip: %+v", u)
	}
	gotU, err := s.UserByUsername(ctx, "contract-user")
	if err != nil || gotU == nil || gotU.ID != u.ID {
		t.Fatalf("user by username: %+v err=%v", gotU, err)
	}
	if err := s.UpdateUserRole(ctx, u.ID, model.RoleAdmin); err != nil {
		t.Fatalf("update user role: %v", err)
	}
	gotU, err = s.UserByUsername(ctx, "contract-user")
	if err != nil || gotU.Role != model.RoleAdmin {
		t.Fatalf("role sync not persisted: %+v err=%v", gotU, err)
	}
	// Sessions join the user row: a session minted AFTER the role change
	// must carry the new role (middleware reads it per request).
	if err := s.CreateSession(ctx, "contract-session-hash", u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess, err := s.SessionByTokenHash(ctx, "contract-session-hash")
	if err != nil || sess == nil {
		t.Fatalf("session by hash: %+v err=%v", sess, err)
	}
	if sess.Username != "contract-user" || sess.Role != model.RoleAdmin {
		t.Fatalf("session role propagation: %+v", sess)
	}
	if err := s.DeleteSession(ctx, "contract-session-hash"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if sess, err := s.SessionByTokenHash(ctx, "contract-session-hash"); err != nil || sess != nil {
		t.Fatalf("revoked session still resolvable: %+v err=%v", sess, err)
	}
}

// TestContractSQLite runs the contract suite against a tmpfile SQLite store.
func TestContractSQLite(t *testing.T) {
	openFn := func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(filepath.Join(t.TempDir(), "contract.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	storeContract(t, openFn)
}

// TestContractPostgres runs the contract suite against the Postgres backend
// when ENCODE_TEST_PG_DSN is set. The DSN must point at the encode_test
// database; the test truncates all tables in t.Cleanup so each run starts
// clean. When the env var is unset, the test is skipped (default `go test ./...`
// needs no Postgres).
func TestContractPostgres(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG contract test")
	}
	if !isPostgresDSN(dsn) {
		t.Fatalf("ENCODE_TEST_PG_DSN must be a postgres:// DSN, got %q", dsn)
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	// Truncate all tables in cleanup so the contract suite starts from an
	// empty database. schema_migrations is left (it's bookkeeping).
	t.Cleanup(func() {
		tables := []string{
			"node_metrics", "audit_events", "pairing_codes", "provision_runs",
			"sessions", "users", "api_tokens", "shares", "settings",
			"step_templates", "series", "jobs", "flows", "nodes",
		}
		ctx := context.Background()
		for _, tbl := range tables {
			if _, err := s.db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl)); err != nil {
				t.Logf("truncate %s: %v (continuing)", tbl, err)
			}
		}
		s.Close()
	})

	openFn := func(t *testing.T) *Store {
		t.Helper()
		return s
	}
	storeContract(t, openFn)
}

// TestAssignJobConcurrencyContract verifies the per-node slot guard under
// concurrent claimers: with max_concurrent_jobs = 1, two goroutines claiming
// the same pending job must result in exactly one assignment. Run on both
// backends (PG env-gated).
func TestAssignJobConcurrencyContract(t *testing.T) {
	storeContractConcurrency(t, func(t *testing.T) *Store {
		s, err := Open(filepath.Join(t.TempDir(), "conc.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestAssignJobConcurrencyContractPG(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG concurrency test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"jobs", "flows", "nodes"} {
			s.db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl))
		}
		s.Close()
	})
	storeContractConcurrency(t, func(t *testing.T) *Store { return s })
}

// storeContractConcurrency tests that two concurrent claimers of the same
// pending job result in exactly one assignment (the one-job-per-node guard).
func storeContractConcurrency(t *testing.T, openFn func(t *testing.T) *Store) {
	ctx := context.Background()
	s := openFn(t)
	flow, _ := s.CreateFlow(ctx, &model.Flow{Name: "conc-flow", Steps: []model.Step{{Type: model.StepEncode}}})
	node, _ := s.CreateNode(ctx, "conc-node", "h")
	j1, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "01", EpisodeDir: "S/Ep 01", ScriptType: "vpy", FlowID: flow.ID})
	j2, _ := s.CreateJob(ctx, &model.Job{Series: "S", Episode: "02", EpisodeDir: "S/Ep 02", ScriptType: "vpy", FlowID: flow.ID})

	// Assign j1 to fill the node's single slot.
	if err := s.AssignJob(ctx, j1.ID, node.ID); err != nil {
		t.Fatalf("first assign: %v", err)
	}

	// Two goroutines try to assign j2 to the same (full) node — both must fail.
	var wg sync.WaitGroup
	var mu sync.Mutex
	successCount := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.AssignJob(ctx, j2.ID, node.ID); err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 0 {
		t.Fatalf("expected 0 successful assigns to full node, got %d", successCount)
	}

	// Verify only j1 is active.
	active, err := s.ActiveJobForNode(ctx, node.ID)
	if err != nil || active == nil || active.ID != j1.ID {
		t.Fatalf("active job mismatch: %+v err=%v", active, err)
	}

	// Finish j1, free the slot, then j2 should assign.
	if err := s.FinishJob(ctx, j1.ID, model.JobDone, 0, "", nil, ""); err != nil {
		t.Fatal(err)
	}
	s.ReleaseNode(ctx, node.ID)
	if err := s.AssignJob(ctx, j2.ID, node.ID); err != nil {
		t.Fatalf("assign after finish: %v", err)
	}
}

// storeContractFailStalled verifies the guarded stall-fail transition
// (FailStalledJob) on both backends: a stale running job transitions exactly
// once, a concurrently-completed job is never overwritten (outputs intact),
// and fresh progress is never failed.
func storeContractFailStalled(t *testing.T, openFn func(t *testing.T) *Store) {
	ctx := context.Background()
	s := openFn(t)
	flow, err := s.CreateFlow(ctx, &model.Flow{Name: "fs-flow", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.CreateNode(ctx, "fs-node", "h")
	if err != nil {
		t.Fatal(err)
	}
	// The fixture runs several jobs on one node; lift the default 1-slot cap.
	if err := s.SetNodeMaxConcurrent(ctx, node.ID, 8); err != nil {
		t.Fatal(err)
	}
	// runningJob creates an assigned+running job with a known progress value.
	runningJob := func(episode string) *model.Job {
		t.Helper()
		j, err := s.CreateJob(ctx, &model.Job{
			Series: "FailStalledShow", Episode: episode, EpisodeDir: "FailStalledShow/Ep " + episode,
			ScriptType: "vpy", FlowID: flow.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AssignJob(ctx, j.ID, node.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateJobStatus(ctx, j.ID, model.JobRunning, "encode", 42, "tail"); err != nil {
			t.Fatal(err)
		}
		return j
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-1 * time.Hour)
	const stallMsg = "stalled: no progress for 60 minutes"

	// 1. A stale running job transitions: failed, exit -1, stall error,
	// finished_at stamped, progress kept, steering stamp set for retry.
	j1 := runningJob("01")
	if err := s.SeedJobLastProgress(ctx, j1.ID, old); err != nil {
		t.Fatal(err)
	}
	ok, err := s.FailStalledJob(ctx, j1.ID, cutoff, stallMsg)
	if err != nil || !ok {
		t.Fatalf("FailStalledJob(stale running) = %v err=%v, want true", ok, err)
	}
	got, err := s.GetJob(ctx, j1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.ExitCode != -1 || got.Error != stallMsg {
		t.Fatalf("stale job transition wrong: %+v", got)
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at not stamped")
	}
	if got.Progress != 42 {
		t.Fatalf("progress = %v, want 42 (failure must not render as completion)", got.Progress)
	}
	if got.LastFailedNodeID != node.ID {
		t.Fatalf("last_failed_node_id = %d, want %d (retry steering)", got.LastFailedNodeID, node.ID)
	}

	// 2. A second call on the same job is a no-op (double transition guard).
	ok, err = s.FailStalledJob(ctx, j1.ID, cutoff, "stalled: again")
	if err != nil || ok {
		t.Fatalf("second FailStalledJob = %v err=%v, want false (already failed)", ok, err)
	}
	got, _ = s.GetJob(ctx, j1.ID)
	if got.Error != stallMsg || got.RetryCount != 0 {
		t.Fatalf("second call mutated the row: %+v", got)
	}

	// 3. A concurrently-completed job is NEVER overwritten, even with an
	// ancient stamp: status, exit code and outputs survive untouched.
	j2 := runningJob("02")
	if err := s.FinishJobWithReport(ctx, j2.ID, model.JobDone, 0, "",
		[]string{"out.mkv"}, "tail", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedJobLastProgress(ctx, j2.ID, old); err != nil {
		t.Fatal(err)
	}
	ok, err = s.FailStalledJob(ctx, j2.ID, cutoff, stallMsg)
	if err != nil || ok {
		t.Fatalf("FailStalledJob(completed) = %v err=%v, want false", ok, err)
	}
	got, _ = s.GetJob(ctx, j2.ID)
	if got.Status != model.JobDone || got.ExitCode != 0 {
		t.Fatalf("completed job overwritten: %+v", got)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != "out.mkv" {
		t.Fatalf("completed job's outputs wiped: %+v", got.Outputs)
	}

	// 4. Fresh progress is never failed (stamp newer than the cutoff).
	j3 := runningJob("03") // claim stamped last_progress_at=now
	ok, err = s.FailStalledJob(ctx, j3.ID, cutoff, stallMsg)
	if err != nil || ok {
		t.Fatalf("FailStalledJob(fresh) = %v err=%v, want false", ok, err)
	}
	got, _ = s.GetJob(ctx, j3.ID)
	if got.Status != model.JobRunning {
		t.Fatalf("fresh-progress job failed: %+v", got)
	}

	// 5. The fail path does not wipe outputs/log_tail of the stalled row
	// itself (it mirrors FinishJob's column set minus the output write).
	j4 := runningJob("04")
	if _, err := s.qExec(ctx, `UPDATE jobs SET outputs_json=? WHERE id=?`, `["partial.mkv"]`, j4.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedJobLastProgress(ctx, j4.ID, old); err != nil {
		t.Fatal(err)
	}
	ok, err = s.FailStalledJob(ctx, j4.ID, cutoff, stallMsg)
	if err != nil || !ok {
		t.Fatalf("FailStalledJob(stale w/ outputs) = %v err=%v, want true", ok, err)
	}
	got, _ = s.GetJob(ctx, j4.ID)
	if len(got.Outputs) != 1 || got.Outputs[0] != "partial.mkv" {
		t.Fatalf("stalled job's outputs wiped: %+v", got.Outputs)
	}
	if got.LogTail != "tail" {
		t.Fatalf("stalled job's log_tail wiped: %q", got.LogTail)
	}
}

// TestFailStalledJobContract runs the guarded stall-fail contract on SQLite.
func TestFailStalledJobContract(t *testing.T) {
	storeContractFailStalled(t, func(t *testing.T) *Store {
		t.Helper()
		s, err := Open(filepath.Join(t.TempDir(), "failstalled.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// TestFailStalledJobContractPG runs the same contract on Postgres when
// ENCODE_TEST_PG_DSN is set (env-gated like the other PG contract suites).
func TestFailStalledJobContractPG(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping PG FailStalledJob contract test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tbl := range []string{"jobs", "flows", "nodes"} {
			s.db.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s RESTART IDENTITY CASCADE", tbl))
		}
		s.Close()
	})
	storeContractFailStalled(t, func(t *testing.T) *Store { return s })
}

// TestRebindPostgres verifies the ? → $N placeholder rebind with hostile
// inputs: ? inside single-quoted string literals must NOT be rebound.
func TestRebindPostgres(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple one param",
			input: "SELECT * FROM jobs WHERE id = ?",
			want:  "SELECT * FROM jobs WHERE id = $1",
		},
		{
			name:  "multiple params",
			input: "INSERT INTO nodes (name, token_hash) VALUES (?, ?)",
			want:  "INSERT INTO nodes (name, token_hash) VALUES ($1, $2)",
		},
		{
			name:  "question mark inside single-quoted literal",
			input: "WHERE x = 'a?b' AND y = ?",
			want:  "WHERE x = 'a?b' AND y = $1",
		},
		{
			name:  "escaped quote keeps literal state",
			input: "WHERE x = 'a''b?' AND y = ?",
			want:  "WHERE x = 'a''b?' AND y = $1",
		},
		{
			name:  "no params",
			input: "SELECT 1",
			want:  "SELECT 1",
		},
		{
			name:  "LIKE pattern with ? in literal",
			input: "WHERE name LIKE '%test?%' AND id = ?",
			want:  "WHERE name LIKE '%test?%' AND id = $1",
		},
		{
			name:  "params around string literal",
			input: "UPDATE t SET a = ?, b = 'literal?', c = ? WHERE id = ?",
			want:  "UPDATE t SET a = $1, b = 'literal?', c = $2 WHERE id = $3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rebindPostgres(tt.input)
			if got != tt.want {
				t.Fatalf("rebindPostgres(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestTranslateDatetimeNow verifies datetime('now') is translated to the PG
// expression. The two-arg modifier form (datetime('now', ?)) is NOT
// translated by this function — it's handled at the call site.
func TestTranslateDatetimeNow(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no-arg form in UPDATE",
			input: "UPDATE jobs SET finished_at=datetime('now') WHERE id=?",
			want:  "UPDATE jobs SET finished_at=to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS') WHERE id=?",
		},
		{
			name:  "multiple occurrences in one query",
			input: "UPDATE t SET a=datetime('now'), b=datetime('now') WHERE id=?",
			want:  "UPDATE t SET a=to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS'), b=to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS') WHERE id=?",
		},
		{
			name:  "two-arg form is NOT translated (handled at call site)",
			input: "WHERE ts < datetime('now', ?)",
			want:  "WHERE ts < datetime('now', ?)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateDatetimeNow(tt.input)
			if got != tt.want {
				t.Fatalf("translateDatetimeNow(%q)\n  got  = %q\n  want = %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestMigratePGFresh proves migratePG creates the full schema on an empty PG
// database. This is the fresh-PG proof: drop+recreate encode_test, then open
// (which runs migratePG), then verify core tables exist and a basic insert
// works. Env-gated like the contract suite.
func TestMigratePGFresh(t *testing.T) {
	dsn := os.Getenv("ENCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("ENCODE_TEST_PG_DSN not set; skipping fresh-PG migration test")
	}
	// Drop and recreate the test database via the maintenance connection.
	if err := dropAndRecreateTestDB(dsn); err != nil {
		t.Fatalf("drop+recreate: %v", err)
	}
	// Open runs migratePG against the empty database.
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("open fresh pg: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	ctx := context.Background()
	// Verify a basic insert works (proves the schema is fully created).
	n, err := s.CreateNode(ctx, "fresh-test", "hash")
	if err != nil {
		t.Fatalf("create node on fresh pg: %v", err)
	}
	if n.ID == 0 {
		t.Fatal("node has no id on fresh pg")
	}
	got, err := s.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatalf("get node on fresh pg: %v", err)
	}
	if got.Name != "fresh-test" {
		t.Fatalf("node name = %q, want fresh-test", got.Name)
	}
	// Verify a settings round-trip (proves the single-row CHECK constraint table).
	if err := s.SaveSettings(ctx, &model.Settings{ControllerURL: "http://fresh"}); err != nil {
		t.Fatalf("save settings on fresh pg: %v", err)
	}
	st, err := s.GetSettings(ctx)
	if err != nil || st == nil || st.ControllerURL != "http://fresh" {
		t.Fatalf("settings round-trip on fresh pg: %+v err=%v", st, err)
	}
}

// dropAndRecreateTestDB drops and recreates the encode_test database via a
// maintenance connection to the default database. It parses the database name
// from the DSN and connects to the "postgres" maintenance database to issue
// the DROP+CREATE. The error from DROP (if the DB doesn't exist yet) is
// tolerated.
func dropAndRecreateTestDB(dsn string) error {
	dbName, maintDSN, err := parseTestDBName(dsn)
	if err != nil {
		return err
	}
	maintDB, err := sql.Open("pgx", maintDSN)
	if err != nil {
		return fmt.Errorf("open maintenance: %w", err)
	}
	defer maintDB.Close()
	// DROP + CREATE. Use fmt to inject the quoted identifier; dbName is parsed
	// from our own DSN, not user input, so this is safe.
	ctx := context.Background()
	_, _ = maintDB.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, dbName))
	if _, err := maintDB.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, dbName)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	return nil
}
