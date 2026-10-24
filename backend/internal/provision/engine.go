// Package provision drives Ansible from the controller to set up fresh
// Windows encode nodes: NFS mounts, toolchain (MediaInfo, AviSynth+,
// Python 64-bit, VapourSynth), agent install as a Windows service, and
// one-shot pairing against this controller.
//
// Security model: the WinRM password arrives per-request, is written ONLY to
// a 0600 temp vars file for the duration of the run, and is removed on
// completion — it is never stored in the database, never logged, and never
// passed on a command line. Pairing codes are issued per-run and live only
// in the same temp file.
package provision

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Request is one provisioning attempt (validated before execution).
type Request struct {
	Host   string
	Port   int
	Scheme string // http | https
	// CertValidation: "validate" | "ignore" for https WinRM server certs.
	// Default https → "ignore" would silently trust MITM'd listeners, so the
	// engine defaults https to "validate"; operators with self-signed node
	// certs opt into "ignore" explicitly (UI checkbox / API field).
	CertValidation string
	WinRMUser      string
	WinRMPassword  string
	NodeName       string
	// Platform selects the provisioning path: "windows" (or "" for back
	// compat with callers that never set it) drives the WinRM/ansible
	// Windows playbook unchanged; "linux" drives the SSH/ansible
	// linux-node playbook. Any other value is rejected.
	Platform string
	// SSH connection details for Linux nodes. SSHUser is required; the
	// auth method is EITHER SSHPassword OR SSHKeyFile (one must be
	// non-empty). SSHPort defaults to 22. These are ignored on Windows.
	SSHUser     string
	SSHPassword string
	SSHKeyFile  string
	// Options.
	InstallToolchain bool // MediaInfo, AviSynth+, Python, VapourSynth
	MountNFS         bool // legacy: NFS client + mounts from Settings (kept for API compat)
	MountShares      bool // mount the enabled shares (smb/nfs rows in the shares table)
	PushBin          bool // extract the published bin package into the bin dir
	// WinRMHTTPS adds an HTTPS listener (5986) to the node during this run
	// so future management can drop plaintext WinRM. Self-signed by default;
	// set WinRMCertThumbprint to reuse a CA-issued cert already on the node.
	WinRMHTTPS          bool
	WinRMCertThumbprint string
}

// isLinux reports whether this request drives the SSH/Linux path.
func (r Request) isLinux() bool { return r.Platform == "linux" }

// Store is the persistence surface the engine needs (narrow for tests).
type Store interface {
	GetSettings(ctx context.Context) (*model.Settings, error)
	// ShareForRole returns the enabled share serving a role (smb preferred
	// over nfs over s3), or nil when none — provisioning then falls back to
	// the legacy NFS Settings fields for that role.
	ShareForRole(ctx context.Context, role model.ShareRole) (*model.Share, error)
	CreateProvisionRun(ctx context.Context, pr *model.ProvisionRun) (*model.ProvisionRun, error)
	GetProvisionRun(ctx context.Context, id int64) (*model.ProvisionRun, error)
	ListProvisionRuns(ctx context.Context) ([]*model.ProvisionRun, error)
	PruneProvisionRuns(ctx context.Context, keep int) error
	SetProvisionRunStatus(ctx context.Context, id int64, status, errMsg string, finished bool) error
	AppendProvisionRunLog(ctx context.Context, id int64, chunk string, capBytes int) error
}

// Payloads stages the agent artifacts for ansible win_copy: agent binary,
// EncodeLib.ps1, and the bin-package zip. Each returns false when that
// payload has not been published yet (the playbook then skips the step).
type Payloads interface {
	StageAgentBinary(dest string) (bool, error)
	StageLib(dest string) (bool, error)
	StageBinZip(dest string) (bool, error)
}

// PairingIssuer mints a one-shot pairing code for the new node. The
// plaintext code goes only into the temp vars file.
type PairingIssuer func(nameHint string) (code string, err error)

// Engine executes provisioning runs. One run at a time per controller
// (parallel ansible runs against the same staging layout invite races); a
// second request queues behind the mutex and the UI sees it as queued.
type Engine struct {
	Store        Store
	Payloads     Payloads
	IssuePairing PairingIssuer
	PlaybookDir  string // directory containing provision.yml
	AnsibleBin   string // ansible-playbook executable (default: PATH lookup)
	LogCap       int    // max bytes of ansible output kept per run

	mu sync.Mutex // serializes runs
}

// ReconcileStaleRuns marks any run left in queued/running state as failed.
// Call once at controller startup: those runs belonged to the previous
// process (crash, restart, redeploy) and will never finish — without this
// they would show as "running" forever.
func (e *Engine) ReconcileStaleRuns(ctx context.Context) error {
	// Crash hygiene first: a controller that died mid-run leaves
	// /tmp/provision-*/ behind, and its vars.yml still holds the WinRM
	// password. Runs never survive a process restart (nothing resumes
	// them), so every such dir is by definition stale — sweep them all.
	entries, err := os.ReadDir(os.TempDir())
	if err == nil {
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), "provision-") {
				_ = os.RemoveAll(filepath.Join(os.TempDir(), en.Name()))
			}
		}
	}

	runs, err := e.Store.ListProvisionRuns(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, r := range runs {
		if r.Status == "queued" || r.Status == "running" {
			// Use a generous cap: these appends go through the same
			// bounded-append path as live output.
			_ = e.Store.AppendProvisionRunLog(ctx, r.ID,
				"\n[controller] controller restarted while this run was active; marking failed\n", e.logCap())
			if err := e.Store.SetProvisionRunStatus(ctx, r.ID, "failed", "interrupted by controller restart", true); err != nil && firstErr == nil {
				firstErr = err // keep going: one bad row must not freeze the rest
			}
		}
	}
	return firstErr
}

const (
	defaultLogCap    = 512 * 1024
	runTimeout       = 45 * time.Minute
	logFlushInterval = 2 * time.Second
)

// hostRe is the allow-list for WinRM targets: hostname chars, dots, colons
// (IPv6), no whitespace or shell metacharacters. Args are passed as argv
// (never through a shell), but validation keeps errors loud and early.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]+$`)

// nodeNameRe is the allow-list for provisioned node names (also the name a
// node registers under). Compiled once, not per Validate call.
var nodeNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Validate checks the request shape before any resources are allocated.
func (r *Request) Validate() error {
	r.Platform = strings.ToLower(r.Platform)
	if r.Platform == "" {
		r.Platform = "windows"
	}
	if r.Platform != "windows" && r.Platform != "linux" {
		return fmt.Errorf("platform must be 'windows' or 'linux'")
	}
	if !hostRe.MatchString(r.Host) {
		return fmt.Errorf("invalid host %q", r.Host)
	}
	if r.NodeName == "" {
		return fmt.Errorf("node_name is required")
	}
	if !nodeNameRe.MatchString(r.NodeName) {
		return fmt.Errorf("node_name %q contains invalid characters", r.NodeName)
	}
	if r.isLinux() {
		return r.validateLinux()
	}
	return r.validateWindows()
}

// validateWindows checks the historical WinRM shape — unchanged behavior.
func (r *Request) validateWindows() error {
	if r.Port == 0 {
		r.Port = 5985
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("invalid port %d", r.Port)
	}
	r.Scheme = strings.ToLower(r.Scheme)
	if r.Scheme == "" {
		r.Scheme = "http"
	}
	if r.Scheme != "http" && r.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	// Cert validation defaults: https → validate (explicit opt-in for
	// self-signed "ignore"); http → ignore is irrelevant (no cert) but
	// pywinrm still wants a legal value.
	r.CertValidation = strings.ToLower(r.CertValidation)
	if r.CertValidation == "" {
		if r.Scheme == "https" {
			r.CertValidation = "validate"
		} else {
			r.CertValidation = "ignore"
		}
	}
	if r.CertValidation != "validate" && r.CertValidation != "ignore" {
		return fmt.Errorf("cert_validation must be 'validate' or 'ignore'")
	}
	if r.WinRMUser == "" {
		r.WinRMUser = "Administrator"
	}
	if r.WinRMPassword == "" {
		return fmt.Errorf("winrm_password is required")
	}
	return nil
}

// validateLinux checks the SSH shape: a user and exactly one auth method
// (password or key file). Port defaults to 22. No WinRM fields are
// consulted.
func (r *Request) validateLinux() error {
	if r.Port == 0 {
		r.Port = 22
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("invalid port %d", r.Port)
	}
	if r.SSHUser == "" {
		return fmt.Errorf("ssh_user is required for linux nodes")
	}
	if r.SSHPassword == "" && strings.TrimSpace(r.SSHKeyFile) == "" {
		return fmt.Errorf("linux nodes require ssh_password or ssh_key_file")
	}
	return nil
}

// Start validates, records, and launches a run in a background goroutine.
// Returns the created run row immediately (the UI polls its status/log).
func (e *Engine) Start(ctx context.Context, req Request) (*model.ProvisionRun, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if e.IssuePairing == nil || e.Payloads == nil {
		return nil, fmt.Errorf("provision engine not fully wired")
	}
	pr, err := e.Store.CreateProvisionRun(ctx, &model.ProvisionRun{
		Host: req.Host, Port: req.Port, Scheme: req.Scheme,
		WinRMUser: req.WinRMUser, NodeName: req.NodeName,
		Status: "queued",
		OptionsJSON: fmt.Sprintf(`{"install_toolchain":%v,"mount_nfs":%v,"mount_shares":%v,"push_bin":%v}`,
			req.InstallToolchain, req.MountNFS, req.MountShares, req.PushBin),
	})
	if err != nil {
		return nil, err
	}
	go e.run(pr.ID, req)
	// Retention: prune old finished runs so the runs table (each row holds a
	// full log) does not grow without bound. Best-effort, never fatal.
	_ = e.Store.PruneProvisionRuns(ctx, 50)
	return pr, nil
}

// run executes one provisioning attempt end to end.
func (e *Engine) run(id int64, req Request) {
	// Serialize runs: queue behind any in-flight run.
	e.mu.Lock()
	defer e.mu.Unlock()

	// Timeout starts only once this run owns the stage: a queued run must
	// not burn its 45-minute budget waiting on the mutex (which would turn
	// into a spurious "provisioning timed out" the moment it starts).
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	bg := context.Background()
	_ = e.Store.SetProvisionRunStatus(bg, id, "running", "", false)

	err := e.execute(ctx, bg, id, req)
	capB := e.logCap()
	if err != nil {
		_ = e.Store.AppendProvisionRunLog(bg, id, "\n[controller] FAILED: "+err.Error()+"\n", capB)
		_ = e.Store.SetProvisionRunStatus(bg, id, "failed", err.Error(), true)
		return
	}
	_ = e.Store.AppendProvisionRunLog(bg, id, "\n[controller] SUCCESS: node provisioned\n", capB)
	_ = e.Store.SetProvisionRunStatus(bg, id, "success", "", true)
}

// execute dispatches to the platform-specific provisioning path.
func (e *Engine) execute(ctx, bg context.Context, id int64, req Request) error {
	if req.isLinux() {
		return e.executeLinux(ctx, bg, id, req)
	}
	return e.executeWindows(ctx, bg, id, req)
}

// loadProvisionCtx resolves settings + per-role shares and checks the
// controller URL. Shared by both platforms before staging.
func (e *Engine) loadProvisionCtx(ctx context.Context) (*model.Settings, *model.Share, *model.Share, error) {
	settings, err := e.Store.GetSettings(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load settings: %w", err)
	}
	if settings == nil {
		return nil, nil, nil, fmt.Errorf("no settings configured (open the Settings page first)")
	}
	if strings.TrimSpace(settings.ControllerURL) == "" {
		return nil, nil, nil, fmt.Errorf("settings.controller_url is empty (set it on the Settings page; nodes must be able to reach this URL)")
	}
	scriptsShare, err := e.Store.ShareForRole(ctx, model.ShareRoleScripts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve scripts share: %w", err)
	}
	releaseShare, err := e.Store.ShareForRole(ctx, model.ShareRoleRelease)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve release share: %w", err)
	}
	return settings, scriptsShare, releaseShare, nil
}

// stagePayloads copies the agent binary, lib, and optional bin zip from
// the update store into the run dir's files/ tree. agentName is the
// platform-specific binary filename (encode-agent.exe on Windows,
// encode-agent on Linux). Returns (libOK, binOK).
func (e *Engine) stagePayloads(dir, agentName string, req Request) (libOK, binOK bool, err error) {
	agentOK, err := e.Payloads.StageAgentBinary(filepath.Join(dir, "files", "agent", agentName))
	if err != nil {
		return false, false, fmt.Errorf("stage agent binary: %w", err)
	}
	if !agentOK {
		return false, false, fmt.Errorf("no agent binary published (publish one on Settings > Push to nodes first)")
	}
	libOK, err = e.Payloads.StageLib(filepath.Join(dir, "files", "agent", "EncodeLib.ps1"))
	if err != nil {
		return false, false, fmt.Errorf("stage EncodeLib: %w", err)
	}
	if req.PushBin {
		binOK, err = e.Payloads.StageBinZip(filepath.Join(dir, "files", "bin-package.zip"))
		if err != nil {
			return false, false, fmt.Errorf("stage bin package: %w", err)
		}
		if !binOK {
			return false, false, fmt.Errorf("push_bin requested but no bin package is published")
		}
	}
	return libOK, binOK, nil
}

// executeWindows performs staging, runs the WinRM ansible playbook, and
// streams the output. Unchanged from the original Windows path.
func (e *Engine) executeWindows(ctx, bg context.Context, id int64, req Request) error {
	settings, scriptsShare, releaseShare, err := e.loadProvisionCtx(ctx)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "provision-*")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// 1. Stage payloads from the update store.
	libOK, binOK, err := e.stagePayloads(dir, "encode-agent.exe", req)
	if err != nil {
		return err
	}

	// 2. Pairing code (single-use bootstrap, plaintext only in vars.yml).
	code, err := e.IssuePairing(req.NodeName)
	if err != nil {
		return fmt.Errorf("issue pairing code: %w", err)
	}

	// 3. Inventory + vars (0600; the password never leaves this file).
	inv := fmt.Sprintf(`[encode_nodes]
%s ansible_host=%s ansible_port=%d ansible_winrm_scheme=%s
`, req.NodeName, req.Host, req.Port, req.Scheme)
	if err := os.WriteFile(filepath.Join(dir, "inventory.yml"), []byte(inv), 0o600); err != nil {
		return err
	}
	vars := buildVars(settings, req, code, libOK, binOK, scriptsShare, releaseShare)
	if err := os.WriteFile(filepath.Join(dir, "vars.yml"), []byte(vars), 0o600); err != nil {
		return err
	}

	// 4. Copy the playbook INTO the run dir: it resolves staged payloads via
	//    {{ playbook_dir }}/files/..., so it must sit next to them.
	srcPlaybook := filepath.Join(e.PlaybookDir, "provision.yml")
	playbook := filepath.Join(dir, "provision.yml")
	if b, err := os.ReadFile(srcPlaybook); err != nil {
		return fmt.Errorf("playbook missing: %w", err)
	} else if err := os.WriteFile(playbook, b, 0o600); err != nil {
		return err
	}
	ansibleBin := e.AnsibleBin
	if ansibleBin == "" {
		ansibleBin = "ansible-playbook"
	}
	// Startup marker: proves log-appends reach the DB before ansible even
	// starts, so an empty final log is never ambiguous (it means ansible
	// produced nothing, not that the plumbing dropped it).
	_ = e.Store.AppendProvisionRunLog(bg, id,
		"[controller] launching ansible-playbook ("+ansibleBin+")\n", e.logCap())
	cmd := exec.CommandContext(ctx, ansibleBin,
		"-i", filepath.Join(dir, "inventory.yml"),
		"-e", "@"+filepath.Join(dir, "vars.yml"),
		playbook)
	cmd.Env = append(os.Environ(),
		"ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_PYTHON_INTERPRETER=auto_silent",
		// Generous connection timeouts: Windows feature installs and large
		// win_copy transfers can make tasks long.
		"ANSIBLE_TIMEOUT=120",
	)
	// Defense in depth: ansible at verbosity or a task echoing vars could
	// reflect secrets into the output; scrub them before they reach the DB.
	redact := []string{req.WinRMPassword, code}
	return streamCommand(ctx, bg, e.Store, id, cmd, e.logCap(), redact)
}

// executeLinux provisions a Linux node over SSH using the linux-node
// playbook. The SSH password and any CIFS share credentials flow ONLY
// through a 0600 JSON vars file passed as `-e @vars.json` (never on a
// command line: a `$` in a password breaks shell/env passing, and command
// lines are visible in /proc). The vars file is shredded by the run-dir
// cleanup (defer os.RemoveAll).
func (e *Engine) executeLinux(ctx, bg context.Context, id int64, req Request) error {
	settings, scriptsShare, releaseShare, err := e.loadProvisionCtx(ctx)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "provision-*")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// 1. Stage payloads (agent binary has no .exe on Linux).
	libOK, binOK, err := e.stagePayloads(dir, "encode-agent", req)
	if err != nil {
		return err
	}

	// 1b. Stage the canonical systemd unit from the repo so the role deploys
	// the SAME file infra/systemd/encode-agent.service defines (single source
	// of truth: no inline unit in the role that can drift). The unit carries
	// After= ordering on automount units (mnt-*.automount); systemd ignores
	// After= on units that don't exist, so the static file is safe on
	// shareless nodes too. The role copies it from
	// {{ playbook_dir }}/files/encode-agent.service.
	if err := e.stageSystemdUnit(dir); err != nil {
		return err
	}

	// 2. Pairing code (single-use bootstrap, plaintext only in vars.json).
	code, err := e.IssuePairing(req.NodeName)
	if err != nil {
		return fmt.Errorf("issue pairing code: %w", err)
	}

	// 3. Inventory: SSH connection, one host. ansible_become is in vars.
	inv := fmt.Sprintf("[encode_nodes]\n%s ansible_host=%s ansible_port=%d\n",
		req.NodeName, req.Host, req.Port)
	if err := os.WriteFile(filepath.Join(dir, "inventory.yml"), []byte(inv), 0o600); err != nil {
		return err
	}

	// 4. Vars as a 0600 JSON file (not env/YAML on a command line): a `$`
	//    in an SMB password breaks env-file passing; JSON is robust. The
	//    file is the ONLY channel for the SSH + CIFS passwords.
	varsJSON := buildVars(settings, req, code, libOK, binOK, scriptsShare, releaseShare)
	varsPath := filepath.Join(dir, "vars.json")
	if err := os.WriteFile(varsPath, []byte(varsJSON), 0o600); err != nil {
		return err
	}

	// 5. Copy the linux-node playbook + role INTO the run dir so the
	//    playbook resolves staged payloads via {{ playbook_dir }}/files/
	//    and its roles/ import.
	srcPlaybook := filepath.Join(e.PlaybookDir, "linux-node.yml")
	playbook := filepath.Join(dir, "linux-node.yml")
	if b, err := os.ReadFile(srcPlaybook); err != nil {
		return fmt.Errorf("linux-node playbook missing: %w", err)
	} else if err := os.WriteFile(playbook, b, 0o600); err != nil {
		return err
	}
	rolesSrc := filepath.Join(e.PlaybookDir, "roles", "linux-node")
	rolesDest := filepath.Join(dir, "roles", "linux-node")
	if err := copyDir(rolesSrc, rolesDest); err != nil {
		return fmt.Errorf("copy linux-node role: %w", err)
	}

	ansibleBin := e.AnsibleBin
	if ansibleBin == "" {
		ansibleBin = "ansible-playbook"
	}
	_ = e.Store.AppendProvisionRunLog(bg, id,
		"[controller] launching ansible-playbook for linux node ("+ansibleBin+")\n", e.logCap())
	cmd := exec.CommandContext(ctx, ansibleBin,
		"-i", filepath.Join(dir, "inventory.yml"),
		"-e", "@"+varsPath,
		playbook)
	cmd.Env = append(os.Environ(),
		"ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_PYTHON_INTERPRETER=auto_silent",
		"ANSIBLE_TIMEOUT=120",
	)
	// Redact every secret that could reach the log: the SSH password, the
	// pairing code, and each CIFS share password (the playbook prints mount
	// commands whose creds file path is fine, but a stray `mount` could
	// echo a password; scrub all of them). Empty strings are filtered: a
	// blank entry would match-and-replace nothing but clutters the list, and
	// strings.ReplaceAll("", ...) is a no-op that still iterates.
	redact := filterNonEmpty(req.SSHPassword, code)
	redact = append(redact, linuxCIFSPasswords(req, scriptsShare, releaseShare)...)
	return streamCommand(ctx, bg, e.Store, id, cmd, e.logCap(), redact)
}

// filterNonEmpty returns the non-empty values among ss. Used to keep the
// redact list free of no-op blank entries (the SSH password may be empty
// when key-file auth is used, and pairing codes are never empty).
func filterNonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// linuxCIFSPasswords collects every CIFS (SMB) password that could appear
// in ansible output for a Linux run, so the redact list covers them all.
func linuxCIFSPasswords(req Request, scriptsShare, releaseShare *model.Share) []string {
	var out []string
	if !req.MountShares {
		return out
	}
	if sh := scriptsShare; sh != nil && sh.Kind == model.ShareSMB && sh.Password != "" {
		out = append(out, sh.Password)
	}
	if sh := releaseShare; sh != nil && sh.Kind == model.ShareSMB && sh.Password != "" {
		out = append(out, sh.Password)
	}
	return out
}

// stageSystemdUnit copies the canonical systemd unit file from the repo
// into the run dir's files/ tree so the linux-node role can deploy it via
// ansible.builtin.copy (single source of truth — the role never inlines a
// divergent unit). The unit is looked up next to PlaybookDir first
// (infra/ansible/../systemd/encode-agent.service in dev, or wherever the
// provision bundle ships it) and falls back to a repo-root relative path.
// Missing unit is a hard error: provisioning without it would silently
// deploy a stale inline unit.
func (e *Engine) stageSystemdUnit(runDir string) error {
	const unitName = "encode-agent.service"
	destDir := filepath.Join(runDir, "files")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("create files dir: %w", err)
	}
	dest := filepath.Join(destDir, unitName)
	for _, src := range []string{
		filepath.Join(e.PlaybookDir, "..", "systemd", unitName),
		filepath.Join(e.PlaybookDir, "systemd", unitName),
		filepath.Join(e.PlaybookDir, "..", "..", "infra", "systemd", unitName),
	} {
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		return os.WriteFile(dest, b, 0o644)
	}
	return fmt.Errorf("systemd unit not found near PlaybookDir %q (expected infra/systemd/%s alongside it)", e.PlaybookDir, unitName)
}

// copyDir recursively copies src to dst, preserving file permissions. Used
// to stage the linux-node ansible role next to the playbook copy in the
// run dir (ansible resolves roles/ relative to playbook_dir).
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("copyDir: %s is not a directory", src)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, en := range entries {
		s := filepath.Join(src, en.Name())
		d := filepath.Join(dst, en.Name())
		if en.IsDir() {
			if err := copyDir(s, d); err != nil {
				return err
			}
		} else {
			fi, err := en.Info()
			if err != nil {
				return err
			}
			in, err := os.Open(s)
			if err != nil {
				return err
			}
			out, err := os.OpenFile(d, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
			if err != nil {
				in.Close()
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				in.Close()
				out.Close()
				return err
			}
			in.Close()
			out.Close()
		}
	}
	return nil
}

func (e *Engine) logCap() int {
	if e.LogCap > 0 {
		return e.LogCap
	}
	return defaultLogCap
}

// streamCommand runs cmd and appends combined output to the run log in
// time-bounded chunks (the DB gets one append per flush window, not per
// line). Context cancellation kills the process via CommandContext.
//
// Output plumbing uses an OS pipe (not io.Pipe): the child inherits the
// write end and os/exec closes the parent's copy after Start, so the read
// side reaches EOF when the child exits. Stderr is merged into the same
// pipe — ansible reports fatal errors on stderr, so discarding it (as a
// naive StdoutPipe setup does) would leave runs dying silently.
func streamCommand(ctx, bg context.Context, st Store, id int64, cmd *exec.Cmd, capBytes int, redact []string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return fmt.Errorf("start ansible-playbook: %w", err)
	}
	// Parent's write end must close so EOF depends only on the child.
	pw.Close()

	// Single-flusher design: the scanner goroutine (this one) and the ticker
	// goroutine both produce output, but ONLY the flusher goroutine ever
	// talks to the store — appends stay ordered and nothing races the final
	// flush. Chunks are delivered on a channel; a failed append is re-queued
	// in place (strings.Builder by value is never copied — that panics).
	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	flushCh := make(chan string, 64)
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		for chunk := range flushCh {
			if chunk == "" {
				continue
			}
			for _, secret := range redact {
				if secret != "" {
					chunk = strings.ReplaceAll(chunk, secret, "[REDACTED]")
				}
			}
			if err := st.AppendProvisionRunLog(bg, id, chunk, capBytes); err != nil {
				// Transient DB failure: re-queue ahead of whatever arrived
				// since, then the next chunk retries it.
				mu.Lock()
				merged := chunk + buf.String()
				buf.Reset()
				buf.WriteString(merged)
				mu.Unlock()
			}
		}
	}()

	drainBuf := func() {
		mu.Lock()
		chunk := buf.String()
		buf.Reset()
		mu.Unlock()
		if chunk != "" {
			flushCh <- chunk
		}
	}

	tickerStop := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		tick := time.NewTicker(logFlushInterval)
		defer tick.Stop()
		for {
			select {
			case <-tickerStop:
				return
			case <-tick.C:
				drainBuf()
			}
		}
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		mu.Lock()
		// capBytes bounds the in-memory buffer; the persisted column is
		// bounded by the same cap inside AppendProvisionRunLog (tail kept —
		// that is where the failure is).
		if buf.Len()+len(line) > capBytes {
			excess := buf.Len() + len(line) - capBytes
			s := buf.String()
			if excess < len(s) {
				buf.Reset()
				buf.WriteString(s[excess:])
			} else {
				buf.Reset()
			}
		}
		buf.WriteString(line)
		mu.Unlock()
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// Oversized line or read error: do NOT abandon the pipe — drain the
		// rest so the child never blocks writing into a full buffer.
		mu.Lock()
		buf.WriteString("\n[controller] WARNING: log capture truncated (" + scanErr.Error() + ")\n")
		mu.Unlock()
		go func() { _, _ = io.Copy(io.Discard, pr) }()
	}
	pr.Close()
	close(tickerStop)
	<-tickerDone // ticker fully stopped before the final flush
	drainBuf()   // final flush goes through the same ordered path
	close(flushCh)
	<-flusherDone

	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("provisioning timed out after %s", runTimeout)
	}
	if waitErr != nil {
		return fmt.Errorf("ansible-playbook exited with an error (see log)")
	}
	return nil
}

// buildVars renders the ansible extra-vars file from live settings + the
// request + the resolved per-role shares (nil = fall back to the legacy NFS
// Settings fields for that role). It dispatches on Platform: Windows
// requests produce YAML (the historical contract, unchanged), Linux
// requests produce JSON (robust against `$` in SMB passwords — a JSON
// string value carries any character, unlike env/YAML command-line args).
// The file is written 0600 and deleted after the run either way.
func buildVars(s *model.Settings, req Request, pairingCode string, libOK, binOK bool,
	scriptsShare, releaseShare *model.Share) string {
	if req.isLinux() {
		return buildVarsLinux(s, req, pairingCode, libOK, binOK, scriptsShare, releaseShare)
	}
	return buildVarsWindows(s, req, pairingCode, libOK, binOK, scriptsShare, releaseShare)
}

// resolveMountAndNFS computes the shared mount flag and legacy NFS export
// strings used by both Windows and Linux var renderers. A share owning a
// role SUPPRESSES the legacy NFS export for that role (one transport per
// role).
func resolveMountAndNFS(s *model.Settings, req Request, scriptsShare, releaseShare *model.Share) (mount bool, nfsScripts, nfsRelease string) {
	mount = req.MountShares || req.MountNFS
	server := strings.TrimSpace(s.NFSServer)
	if mount && server != "" {
		if sp := strings.TrimSpace(s.ScriptsShare); sp != "" && !shareOwnsRole(scriptsShare) {
			nfsScripts = server + ":" + sp
		}
		if rp := strings.TrimSpace(s.ReleaseShare); rp != "" && !shareOwnsRole(releaseShare) {
			nfsRelease = server + ":" + rp
		}
	}
	return mount, nfsScripts, nfsRelease
}

// buildVarsWindows renders the historical YAML vars file for WinRM targets.
// Unchanged behavior: existing tests assert its exact output.
func buildVarsWindows(s *model.Settings, req Request, pairingCode string, libOK, binOK bool,
	scriptsShare, releaseShare *model.Share) string {
	mount, nfsScripts, nfsRelease := resolveMountAndNFS(s, req, scriptsShare, releaseShare)
	// SMB vars: only a smb-kind share renders them (s3 shares have no
	// mount — the agent transfers per job).
	smbScriptsUNC, smbScriptsUser, smbScriptsPass := "", "", ""
	smbReleaseUNC, smbReleaseUser, smbReleasePass := "", "", ""
	mountSMB := false
	if mount {
		if sh := scriptsShare; sh != nil && sh.Kind == model.ShareSMB {
			smbScriptsUNC = smbUNC(sh)
			smbScriptsUser, smbScriptsPass = sh.Username, sh.Password
			mountSMB = true
		}
		if sh := releaseShare; sh != nil && sh.Kind == model.ShareSMB {
			smbReleaseUNC = smbUNC(sh)
			smbReleaseUser, smbReleasePass = sh.Username, sh.Password
			mountSMB = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "encode_controller_url: %s\n", yamlQuote(s.ControllerURL))
	fmt.Fprintf(&b, "encode_node_name: %s\n", yamlQuote(req.NodeName))
	fmt.Fprintf(&b, "encode_bin_dir: %s\n", yamlQuote(firstNonEmpty(s.NodeBinDir, `C:\bin`)))
	fmt.Fprintf(&b, "encode_scripts_mount: %s\n", yamlQuote(firstNonEmpty(s.NodeScriptsDir, `C:\Encodes\scripts`)))
	fmt.Fprintf(&b, "encode_release_mount: %s\n", yamlQuote(firstNonEmpty(s.NodeReleaseDir, `C:\Encodes\ReleaseFolders`)))
	fmt.Fprintf(&b, "encode_agent_dir: %s\n", yamlQuote(`C:\encode-agent`))
	fmt.Fprintf(&b, "encode_heartbeat_seconds: 15\n")
	fmt.Fprintf(&b, "encode_nfs_scripts_export: %s\n", yamlQuote(nfsScripts))
	fmt.Fprintf(&b, "encode_nfs_release_export: %s\n", yamlQuote(nfsRelease))
	fmt.Fprintf(&b, "encode_mount_nfs: %v\n", mount && (nfsScripts != "" || nfsRelease != ""))
	fmt.Fprintf(&b, "encode_smb_scripts_unc: %s\n", yamlQuote(smbScriptsUNC))
	fmt.Fprintf(&b, "encode_smb_release_unc: %s\n", yamlQuote(smbReleaseUNC))
	fmt.Fprintf(&b, "encode_smb_scripts_user: %s\n", yamlQuote(smbScriptsUser))
	fmt.Fprintf(&b, "encode_smb_scripts_pass: %s\n", yamlQuote(smbScriptsPass))
	fmt.Fprintf(&b, "encode_smb_release_user: %s\n", yamlQuote(smbReleaseUser))
	fmt.Fprintf(&b, "encode_smb_release_pass: %s\n", yamlQuote(smbReleasePass))
	fmt.Fprintf(&b, "encode_mount_smb: %v\n", mountSMB)
	fmt.Fprintf(&b, "encode_install_toolchain: %v\n", req.InstallToolchain)
	// WinRM HTTPS listener bootstrap (opt-in; self-signed unless a thumbprint
	// is supplied). See infra/ansible/tasks/winrm-https.yml.
	fmt.Fprintf(&b, "encode_winrm_https: %v\n", req.WinRMHTTPS)
	fmt.Fprintf(&b, "encode_winrm_cert_thumbprint: %s\n", yamlQuote(req.WinRMCertThumbprint))
	fmt.Fprintf(&b, "encode_push_bin: %v\n", binOK)
	fmt.Fprintf(&b, "encode_has_lib: %v\n", libOK)
	fmt.Fprintf(&b, "encode_pairing_code: %s\n", yamlQuote(pairingCode))
	fmt.Fprintf(&b, "ansible_user: %s\n", yamlQuote(req.WinRMUser))
	fmt.Fprintf(&b, "ansible_password: %s\n", yamlQuote(req.WinRMPassword))
	fmt.Fprintf(&b, "ansible_connection: winrm\n")
	fmt.Fprintf(&b, "ansible_winrm_transport: ntlm\n")
	// Cert validation: "validate" for https with proper certs; "ignore"
	// only when the operator opted in (self-signed homelab listeners).
	// Validate() defaults https→validate, http→ignore.
	certVal := req.CertValidation
	if certVal == "" {
		certVal = "ignore"
	}
	fmt.Fprintf(&b, "ansible_winrm_server_cert_validation: %s\n", certVal)
	// pywinrm requirement: read_timeout MUST exceed operation_timeout (the
	// read window wraps the operation with slack), both non-zero.
	fmt.Fprintf(&b, "ansible_winrm_read_timeout_sec: 150\n")
	fmt.Fprintf(&b, "ansible_winrm_operation_timeout_sec: 120\n")
	return b.String()
}

// buildVarsLinux renders a JSON extra-vars file for SSH/Linux targets. JSON
// (not YAML/env) is chosen deliberately: an SMB password containing `$`
// breaks env-file and shell passing, and YAML single-quote scalars mangle
// values with control characters — a JSON string escapes all of them
// faithfully. The file is passed as `-e @vars.json` (0600) and shredded
// after the run; no secret ever appears on a command line.
//
// CIFS vars mirror the Windows encode_smb_* naming as encode_cifs_*: the
// Linux role mounts each enabled SMB share with a per-share credentials
// file (never on a command line), so the password travels only inside this
// JSON file -> the ansible vars -> a 0600 creds file on the node.
func buildVarsLinux(s *model.Settings, req Request, pairingCode string, libOK, binOK bool,
	scriptsShare, releaseShare *model.Share) string {
	mount, nfsScripts, nfsRelease := resolveMountAndNFS(s, req, scriptsShare, releaseShare)

	// CIFS vars: only smb-kind shares render them (s3 shares have no mount;
	// nfs shares have no credentials).
	type cifsShare struct {
		Server, Share, User, Pass, MountPath string
	}
	var cifsScripts, cifsRelease cifsShare
	mountCIFS := false
	if mount {
		if sh := scriptsShare; sh != nil && sh.Kind == model.ShareSMB {
			cifsScripts = cifsShare{sh.Server, cifsShareName(sh.Path), sh.Username, sh.Password, sh.MountPath}
			mountCIFS = true
		}
		if sh := releaseShare; sh != nil && sh.Kind == model.ShareSMB {
			cifsRelease = cifsShare{sh.Server, cifsShareName(sh.Path), sh.Username, sh.Password, sh.MountPath}
			mountCIFS = true
		}
	}

	v := map[string]any{
		"encode_controller_url":      s.ControllerURL,
		"encode_node_name":           req.NodeName,
		"encode_bin_dir":             firstNonEmpty(s.NodeBinDir, "/opt/encode/bin"),
		"encode_scripts_mount":       firstNonEmpty(s.NodeScriptsDir, "/mnt/encodes/scripts"),
		"encode_release_mount":       firstNonEmpty(s.NodeReleaseDir, "/mnt/encodes/release"),
		"encode_agent_dir":           "/opt/encode-agent",
		"encode_agent_user":          "encode",
		"encode_heartbeat_seconds":   15,
		"encode_nfs_scripts_export":  nfsScripts,
		"encode_nfs_release_export":  nfsRelease,
		"encode_mount_nfs":           mount && (nfsScripts != "" || nfsRelease != ""),
		"encode_cifs_vers":           "3.0",
		"encode_cifs_scripts_server": cifsScripts.Server,
		"encode_cifs_scripts_share":  cifsScripts.Share,
		"encode_cifs_scripts_user":   cifsScripts.User,
		"encode_cifs_scripts_pass":   cifsScripts.Pass,
		"encode_cifs_scripts_mount":  cifsScripts.MountPath,
		"encode_cifs_release_server": cifsRelease.Server,
		"encode_cifs_release_share":  cifsRelease.Share,
		"encode_cifs_release_user":   cifsRelease.User,
		"encode_cifs_release_pass":   cifsRelease.Pass,
		"encode_cifs_release_mount":  cifsRelease.MountPath,
		"encode_mount_cifs":          mountCIFS,
		"encode_install_toolchain":   req.InstallToolchain,
		"encode_push_bin":            binOK,
		"encode_has_lib":             libOK,
		"encode_pairing_code":        pairingCode,
		// SSH connection — ansible ssh plugin reads these. ansible_become
		// runs the agent install/mount tasks as root via sudo.
		"ansible_host":   req.Host,
		"ansible_user":   req.SSHUser,
		"ansible_port":   req.Port,
		"ansible_become": true,
	}
	if req.SSHPassword != "" {
		v["ansible_password"] = req.SSHPassword
	}
	if kf := strings.TrimSpace(req.SSHKeyFile); kf != "" {
		v["ansible_ssh_private_key_file"] = kf
	}
	b, err := json.Marshal(v)
	if err != nil {
		// Should never happen: all values are strings/bools/ints. Fall back
		// to a minimal safe payload rather than crashing a provision run.
		return `{"encode_node_name":"` + req.NodeName + `"}`
	}
	return string(b)
}

// cifsShareName extracts the SMB share name from a share row's Path. The
// Path field holds the share name (e.g. "scripts") for smb-kind shares;
// leading/trailing slashes are trimmed.
func cifsShareName(p string) string {
	return strings.Trim(p, "/\\")
}

// shareOwnsRole reports whether a resolved share (any kind) owns a role —
// used to suppress the legacy NFS export for that role.
func shareOwnsRole(sh *model.Share) bool { return sh != nil }

// smbUNC renders \\server\share from a share row (path may use either
// slash direction; backslashes are canonical for UNC).
func smbUNC(sh *model.Share) string {
	return `\\` + sh.Server + `\` + strings.ReplaceAll(strings.Trim(sh.Path, "/\\"), "/", "\\")
}

// yamlQuote single-quotes a scalar, escaping embedded single quotes. Keeps
// arbitrary settings values (paths with backslashes, etc.) YAML-safe without
// a full marshaler dependency. CR/LF/NUL are stripped: a newline inside a
// single-quoted YAML scalar silently folds (mangling a password) or breaks
// parsing entirely.
func yamlQuote(s string) string {
	s = strings.ReplaceAll(s, "'", "''")
	s = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(s)
	return "'" + s + "'"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
