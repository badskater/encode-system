// Command encodectl is a stdlib-only operator CLI for the encode-system
// controller. It wraps the controller's REST API so routine fleet ops —
// health, scan, node listing, job inspection/lifecycle, retention pruning,
// series/stats/manifest queries, and agent publishing/rollback — can be
// scripted from a shell or CI without the web UI.
//
// Usage model:
//
//	encodectl -server https://ctrl:8443 -token <api-token> health
//	encodectl jobs -status failed -limit 20
//	encodectl publish-agent -platform linux -version 1.4.0 -file ./encode-agent-linux-amd64
//
// Server/token may be supplied via the -server/-token flags or the
// ENCODECTL_SERVER / ENCODECTL_TOKEN environment variables (flags win).
// Missing either is a usage error (exit 2). Non-2xx API responses print the
// server's error message to stderr and exit 1. -json dumps the raw response
// body verbatim instead of the tabular view.
//
// Build: see .github/workflows/release.yml — `go build -ldflags "-X main.Version=…"`
// stamps the release version; "dev" otherwise.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is stamped at build time: -ldflags "-X main.Version=0.1.0".
var Version = "dev"

// envServer / envToken are the environment-variable names for the global
// connection flags, so the defaults can be wired via flag.Lookup without
// duplicating the strings.
const (
	envServer = "ENCODECTL_SERVER"
	envToken  = "ENCODECTL_TOKEN"

	// exitUsage is returned for CLI usage errors (missing flags, unknown
	// command, missing server/token). It deliberately differs from exit 1
	// (API/runtime error) so shells and CI can distinguish "wrong invocation"
	// from "the controller said no".
	exitUsage = 2
)

// doFunc performs one HTTP request against the controller. The body and
// content-type are the request payload (nil/"" for GET/DELETE). Callers
// close the returned response body. main wires a real *http.Client-backed
// implementation that injects the bearer token; tests inject a fake that
// records the call and returns canned responses.
type doFunc func(method, path string, body io.Reader, contentType string) (*http.Response, error)

func main() {
	if err := run(os.Stdout, os.Args[1:], realDo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A usage error already printed its own usage via flag.Failf/Usage;
		// only surface the exit code here.
		if isUsageError(err) {
			os.Exit(exitUsage)
		}
		os.Exit(1)
	}
}

// usageErr tags an error as a usage/invocation error so run's caller (main)
// exits with exitUsage. flag's own error handling already prints usage.
type usageErr struct{ err error }

func (u usageErr) Error() string { return u.err.Error() }

func isUsageError(err error) bool {
	var u usageErr
	return errors.As(err, &u) || errors.Is(err, flag.ErrHelp)
}

// run is the testable entrypoint: it parses args, dispatches a command, and
// writes output to w. do performs the HTTP call (real or fake). It returns a
// nil error on success; a usageErr (exit 2) for invocation problems; any
// other error (exit 1) for API/runtime failures.
func run(w io.Writer, args []string, do doFunc) error {
	if len(args) == 0 {
		return usageErr{err: errors.New("no command given")}
	}

	// Global flags are parsed up front so commands don't each re-implement
	// server/token/json handling. The global FlagSet uses ContinueOnError so
	// a bad flag returns here instead of os.Exit-ing inside the flag package.
	gfs := flag.NewFlagSet("encodectl", flag.ContinueOnError)
	gfs.SetOutput(w)
	server := gfs.String("server", os.Getenv(envServer), "controller base URL (env "+envServer+")")
	token := gfs.String("token", os.Getenv(envToken), "API bearer token (env "+envToken+")")
	rawJSON := gfs.Bool("json", false, "dump raw response body instead of table output")
	if err := gfs.Parse(args); err != nil {
		// flag.ContinueOnError already printed usage for parse failures.
		return usageErr{err: err}
	}

	rest := gfs.Args()
	if len(rest) == 0 {
		return usageErr{err: errors.New("no command given")}
	}
	cmd, rest := rest[0], rest[1:]

	// Local-only commands short-circuit before the server/token check so they
	// work without any connection config (useful in scripts, CI, and `--help`).
	switch cmd {
	case "version":
		fmt.Fprintf(w, "encodectl %s\n", Version)
		return nil
	case "help", "-h", "--help":
		printUsage(w)
		return nil
	}

	// -server/-token are required for every network command. Failing here
	// (before any network call) keeps the exit-code contract clean: a missing
	// connection is an invocation error, not a controller error.
	if *server == "" || *token == "" {
		missing := envServer
		if *server != "" {
			missing = envToken
		}
		return usageErr{err: fmt.Errorf("-%s is required (env %s)", missing, missing)}
	}

	// Publish the parsed connection params to the package-level vars that
	// realDo reads. In tests the injected fake do ignores these entirely,
	// so this side effect is invisible to the test seam.
	globalServer = *server
	globalToken = *token

	ctx := &cliCtx{w: w, do: do, server: *server, token: *token, json: *rawJSON}

	switch cmd {
	case "health":
		return ctx.getAndRender("GET", "/api/health", nil, "", renderHealth)
	case "scan":
		return ctx.getAndRender("POST", "/api/scan", nil, "", renderScan)
	case "nodes":
		return ctx.getAndRender("GET", "/api/nodes", nil, "", renderNodes)
	case "jobs":
		return cmdJobs(ctx, rest)
	case "job-retry":
		return ctx.idAction("POST", "/api/jobs/%s/retry", rest)
	case "job-cancel":
		return ctx.idAction("POST", "/api/jobs/%s/cancel", rest)
	case "job-delete":
		return ctx.idAction("DELETE", "/api/jobs/%s", rest)
	case "prune":
		return cmdPrune(ctx, rest)
	case "series":
		return ctx.getAndRender("GET", "/api/series", nil, "", renderSeries)
	case "stats":
		return ctx.getAndRender("GET", "/api/stats", nil, "", renderStats)
	case "manifest":
		return ctx.getAndRender("GET", "/api/updates/manifest", nil, "", renderManifest)
	case "publish-agent":
		return cmdPublishAgent(ctx, rest)
	case "rollback-agent":
		return cmdRollbackAgent(ctx, rest)
	default:
		return usageErr{err: fmt.Errorf("unknown command %q", cmd)}
	}
}

// cliCtx carries the parsed globals through the command handlers.
type cliCtx struct {
	w      io.Writer
	do     doFunc
	server string
	token  string
	json   bool
}

// getAndRender issues a request, checks the status, and renders the body.
// On -json it copies the raw body verbatim; otherwise render parses the JSON
// and writes a greppable table. A non-2xx response is surfaced as an error
// carrying the server's error message (exit 1).
func (c *cliCtx) getAndRender(method, path string, body io.Reader, ct string, render func(io.Writer, []byte) error) error {
	resp, err := c.do(method, path, body, ct)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, raw)
	}
	if c.json {
		_, _ = c.w.Write(raw)
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			fmt.Fprintln(c.w)
		}
		return nil
	}
	return render(c.w, raw)
}

// idAction runs a simple POST/DELETE against a /api/jobs/{id} sub-path. It
// expects exactly one positional arg (the job id); the table render for
// single-object responses is the generic key/value dump.
func (c *cliCtx) idAction(method, pathTmpl string, rest []string) error {
	if len(rest) != 1 {
		return usageErr{err: fmt.Errorf("usage: encodectl %s <id>", commandFromPath(pathTmpl))}
	}
	id := rest[0]
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return usageErr{err: fmt.Errorf("invalid job id %q: must be an integer", id)}
	}
	path := fmt.Sprintf(pathTmpl, id)
	return c.getAndRender(method, path, nil, "", renderGeneric)
}

// cmdJobs handles `jobs [-status S] [-limit N]`.
func cmdJobs(ctx *cliCtx, rest []string) error {
	fs := flag.NewFlagSet("jobs", flag.ContinueOnError)
	fs.SetOutput(ctx.w)
	status := fs.String("status", "", "filter by status (queued|running|done|failed|cancelled)")
	limit := fs.Int("limit", 0, "max jobs to return (0 = server default)")
	if err := fs.Parse(rest); err != nil {
		return usageErr{err: err}
	}
	path := "/api/jobs"
	q := make([]string, 0, 2)
	if *status != "" {
		q = append(q, "status="+*status)
	}
	if *limit > 0 {
		q = append(q, "limit="+strconv.Itoa(*limit))
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	return ctx.getAndRender("GET", path, nil, "", renderJobs)
}

// cmdPrune handles `prune -days N`.
func cmdPrune(ctx *cliCtx, rest []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(ctx.w)
	days := fs.Int("days", 0, "delete jobs finished more than N days ago (required, 1-3650)")
	if err := fs.Parse(rest); err != nil {
		return usageErr{err: err}
	}
	if *days <= 0 {
		return usageErr{err: errors.New("prune requires -days N (1-3650)")}
	}
	body, _ := json.Marshal(map[string]int{"days": *days})
	return ctx.getAndRender("POST", "/api/jobs/prune", bytes.NewReader(body), "application/json", renderPrune)
}

// cmdPublishAgent handles `publish-agent -platform P -version V -file PATH`.
// It builds a multipart/form-data body with the version, optional platform,
// and file fields and PUTs it to POST /api/updates/agent.
func cmdPublishAgent(ctx *cliCtx, rest []string) error {
	fs := flag.NewFlagSet("publish-agent", flag.ContinueOnError)
	fs.SetOutput(ctx.w)
	platform := fs.String("platform", "", "agent platform: windows|linux (required)")
	version := fs.String("version", "", "agent version string (required)")
	file := fs.String("file", "", "path to the agent binary (required)")
	if err := fs.Parse(rest); err != nil {
		return usageErr{err: err}
	}
	if *platform != "windows" && *platform != "linux" {
		return usageErr{err: errors.New("publish-agent requires -platform windows|linux")}
	}
	if *version == "" {
		return usageErr{err: errors.New("publish-agent requires -version V")}
	}
	if *file == "" {
		return usageErr{err: errors.New("publish-agent requires -file PATH")}
	}

	// Read the file before opening the multipart pipe so a missing/unreadable
	// file is a usage-class error (clear message) rather than a mid-upload
	// network error.
	payload, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("version", *version); err != nil {
		return fmt.Errorf("multipart version field: %w", err)
	}
	if err := mw.WriteField("platform", *platform); err != nil {
		return fmt.Errorf("multipart platform field: %w", err)
	}
	// Use the base name as the filename — the controller keys on the field
	// name ("file"), not the filename, so only the basename is meaningful to
	// a human reading the upload.
	part, err := mw.CreateFormFile("file", filepath.Base(*file))
	if err != nil {
		return fmt.Errorf("create file field: %w", err)
	}
	if _, err := part.Write(payload); err != nil {
		return fmt.Errorf("write file field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	return ctx.getAndRender("POST", "/api/updates/agent", &buf, mw.FormDataContentType(), renderManifest)
}

// cmdRollbackAgent handles `rollback-agent [-platform P]`.
func cmdRollbackAgent(ctx *cliCtx, rest []string) error {
	fs := flag.NewFlagSet("rollback-agent", flag.ContinueOnError)
	fs.SetOutput(ctx.w)
	platform := fs.String("platform", "", "agent platform: windows|linux (default: windows)")
	if err := fs.Parse(rest); err != nil {
		return usageErr{err: err}
	}
	if *platform != "" && *platform != "windows" && *platform != "linux" {
		return usageErr{err: errors.New("rollback-agent -platform must be windows or linux")}
	}
	path := "/api/updates/agent/rollback"
	if *platform != "" {
		path += "?platform=" + *platform
	}
	return ctx.getAndRender("POST", path, nil, "", renderManifest)
}

// --- renderers ----------------------------------------------------------
//
// Each renderer parses the raw JSON body and writes a tab-separated,
// header-prefixed table that stays greppable: `encodectl jobs | grep failed`
// works, and `cut -f1` yields ids. When the body is not the expected shape
// (e.g. the controller returns an unexpected type), the renderer falls back
// to renderGeneric so -json / raw inspection still works.

// renderHealth handles GET /api/health. The body is a status object; we
// print its top-level key/value pairs.
func renderHealth(w io.Writer, raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return renderGeneric(w, raw)
	}
	fmt.Fprintln(w, "KEY\tVALUE")
	keys := sortedKeys(m)
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%v\n", k, m[k])
	}
	return nil
}

// renderScan handles POST /api/scan. The 202 response is {"triggered":true}
// (or an error). Print the acknowledged status.
func renderScan(w io.Writer, raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return renderGeneric(w, raw)
	}
	triggered, _ := m["triggered"].(bool)
	if triggered {
		fmt.Fprintln(w, "scan triggered")
		return nil
	}
	// Unexpected but non-error body — show it generically.
	return renderGeneric(w, raw)
}

// renderNodes handles GET /api/nodes → []Node.
func renderNodes(w io.Writer, raw []byte) error {
	var nodes []struct {
		ID            int64      `json:"id"`
		Name          string     `json:"name"`
		Status        string     `json:"status"`
		Platform      string     `json:"platform"`
		AgentVersion  string     `json:"agent_version"`
		Enabled       bool       `json:"enabled"`
		ActiveJobs    int        `json:"active_jobs"`
		MaxConcurrent int        `json:"max_concurrent_jobs"`
		LastSeen      *time.Time `json:"last_seen"`
	}
	if err := json.Unmarshal(raw, &nodes); err != nil {
		// A single object (not a list) — fall back to generic dump.
		return renderGeneric(w, raw)
	}
	fmt.Fprintln(w, "ID\tNAME\tSTATUS\tPLATFORM\tAGENT_VERSION\tENABLED\tACTIVE\tMAX\tLAST_SEEN")
	for _, n := range nodes {
		last := ""
		if n.LastSeen != nil {
			last = n.LastSeen.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%t\t%d\t%d\t%s\n",
			n.ID, n.Name, n.Status, platLabel(n.Platform), n.AgentVersion,
			n.Enabled, n.ActiveJobs, n.MaxConcurrent, last)
	}
	return nil
}

// renderJobs handles GET /api/jobs → []Job.
func renderJobs(w io.Writer, raw []byte) error {
	var jobs []struct {
		ID         int64      `json:"id"`
		Series     string     `json:"series"`
		Episode    string     `json:"episode"`
		Status     string     `json:"status"`
		NodeID     int64      `json:"node_id"`
		Step       string     `json:"step"`
		Progress   float64    `json:"progress"`
		Error      string     `json:"error"`
		CreatedAt  time.Time  `json:"created_at"`
		FinishedAt *time.Time `json:"finished_at"`
	}
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return renderGeneric(w, raw)
	}
	fmt.Fprintln(w, "ID\tSERIES\tEP\tSTATUS\tNODE\tSTEP\tPROGRESS\tERROR\tCREATED\tFINISHED")
	for _, j := range jobs {
		fin := ""
		if j.FinishedAt != nil {
			fin = j.FinishedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s\t%.0f%%\t%s\t%s\t%s\n",
			j.ID, j.Series, j.Episode, j.Status, j.NodeID, j.Step,
			j.Progress*100, truncField(j.Error, 40), j.CreatedAt.Format(time.RFC3339), fin)
	}
	return nil
}

// renderSeries handles GET /api/series → []Series.
func renderSeries(w io.Writer, raw []byte) error {
	var series []struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		FlowID  int64  `json:"flow_id"`
		Enabled bool   `json:"enabled"`
		Paused  bool   `json:"paused"`
		Tag     string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &series); err != nil {
		return renderGeneric(w, raw)
	}
	fmt.Fprintln(w, "ID\tNAME\tFLOW_ID\tENABLED\tPAUSED\tTAG")
	for _, s := range series {
		fmt.Fprintf(w, "%d\t%s\t%d\t%t\t%t\t%s\n", s.ID, s.Name, s.FlowID, s.Enabled, s.Paused, s.Tag)
	}
	return nil
}

// renderStats handles GET /api/stats. The stats object is free-form enough
// (per-node, per-flow, per-day slices) that a generic top-level dump is the
// most useful greppable form.
func renderStats(w io.Writer, raw []byte) error {
	return renderGeneric(w, raw)
}

// renderManifest handles GET /api/updates/manifest and the publish/rollback
// responses (which echo the manifest).
func renderManifest(w io.Writer, raw []byte) error {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return renderGeneric(w, raw)
	}
	fmt.Fprintln(w, "KEY\tVALUE")
	for _, k := range sortedKeys(m) {
		fmt.Fprintf(w, "%s\t%v\n", k, m[k])
	}
	return nil
}

// renderPrune handles POST /api/jobs/prune → {"deleted":N,"days":N}.
func renderPrune(w io.Writer, raw []byte) error {
	var m struct {
		Deleted int `json:"deleted"`
		Days    int `json:"days"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return renderGeneric(w, raw)
	}
	fmt.Fprintf(w, "pruned %d jobs older than %d days\n", m.Deleted, m.Days)
	return nil
}

// renderGeneric is the fallback: pretty-print any JSON value, or write raw
// text if the body isn't JSON. Used for single-object responses (job-retry,
// job-cancel, job-delete) and unknown shapes.
func renderGeneric(w io.Writer, raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not JSON — write the raw text (already validated non-error by the
		// caller).
		_, _ = w.Write(raw)
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			fmt.Fprintln(w)
		}
		return nil
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- helpers ------------------------------------------------------------

// apiError extracts the server's {"error":"..."} message (if present) and
// wraps it with the HTTP status. The CLI surfaces this to stderr and exits 1.
func apiError(status int, raw []byte) error {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Error != "" {
		return fmt.Errorf("api %d: %s", status, body.Error)
	}
	// No structured error field — show the status and a trimmed body.
	preview := strings.TrimSpace(string(raw))
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	return fmt.Errorf("api %d: %s", status, preview)
}

// sortedKeys returns the keys of a map in sorted order for deterministic
// output (manifest/health key/value dumps).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// sortStrings is a stdlib-free insertion sort for the small key sets we
// handle (≤ ~20 keys). Avoids importing sort for a single trivial use.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// platLabel normalizes the platform field: the model treats "" as "windows"
// (historical fleet default), so the CLI mirrors that for display.
func platLabel(p string) string {
	if p == "" {
		return "windows"
	}
	return p
}

// truncField clips a string to n runes with an ellipsis, for columns that
// can hold long error text.
func truncField(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

// commandFromPath derives a command name from a path template for usage
// messages (e.g. "/api/jobs/%s/retry" → "job-retry").
func commandFromPath(pathTmpl string) string {
	t := strings.TrimPrefix(pathTmpl, "/api/")
	t = strings.TrimSuffix(t, "/")
	t = strings.ReplaceAll(t, "/", "-")
	t = strings.ReplaceAll(t, "%s", "<id>")
	// Collapse the action suffix: jobs/retry → job-retry is the CLI spelling.
	t = strings.ReplaceAll(t, "jobs-", "job-")
	return t
}

// printUsage writes the command summary.
func printUsage(w io.Writer) {
	fmt.Fprintf(w, "encodectl %s — encode-system controller CLI\n\n", Version)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  encodectl [-server URL] [-token TOKEN] [-json] <command> [args]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global flags (env: ENCODECTL_SERVER, ENCODECTL_TOKEN):")
	fmt.Fprintln(w, "  -server   controller base URL")
	fmt.Fprintln(w, "  -token    API bearer token")
	fmt.Fprintln(w, "  -json     dump raw response body instead of tables")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  health                          controller health check")
	fmt.Fprintln(w, "  scan                            trigger a share scan")
	fmt.Fprintln(w, "  nodes                           list fleet nodes")
	fmt.Fprintln(w, "  jobs [-status S] [-limit N]     list jobs")
	fmt.Fprintln(w, "  job-retry <id>                  re-queue a job")
	fmt.Fprintln(w, "  job-cancel <id>                 cancel a pending/assigned job")
	fmt.Fprintln(w, "  job-delete <id>                 delete a job (new API)")
	fmt.Fprintln(w, "  prune -days N                   delete jobs older than N days")
	fmt.Fprintln(w, "  series                          list series")
	fmt.Fprintln(w, "  stats                           fleet stats snapshot")
	fmt.Fprintln(w, "  manifest                        agent update manifest")
	fmt.Fprintln(w, "  publish-agent -platform P -version V -file PATH")
	fmt.Fprintln(w, "                                  upload a new agent binary")
	fmt.Fprintln(w, "  rollback-agent [-platform P]    roll back the current agent")
	fmt.Fprintln(w, "  version                         print encodectl version")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Exit codes: 0 success, 1 API/runtime error, 2 usage error.")
}

// realDo is the production doFunc: it builds the full URL from the base
// server + path, injects the bearer token, and executes the request. It is
// the only function in this file that touches a real network.
func realDo(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	server, token := globalServer, globalToken
	if server == "" || token == "" {
		return nil, errors.New("missing -server/-token")
	}
	url := strings.TrimRight(server, "/") + path
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return http.DefaultClient.Do(req)
}

// globalServer/globalToken are set by main before realDo runs, so realDo has
// the same signature as the test fake (no closure state to thread through
// main). They are written once at startup and read-only thereafter.
var (
	globalServer string
	globalToken  string
)
