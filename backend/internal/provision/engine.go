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
	Host         string
	Port         int
	Scheme       string // http | https
	WinRMUser    string
	WinRMPassword string
	NodeName     string
	// Options.
	InstallToolchain bool // MediaInfo, AviSynth+, Python, VapourSynth
	MountNFS         bool // NFS client + share mounts (needs settings.NFSServer)
	PushBin          bool // extract the published bin package into the bin dir
}

// Store is the persistence surface the engine needs (narrow for tests).
type Store interface {
	GetSettings(ctx context.Context) (*model.Settings, error)
	CreateProvisionRun(ctx context.Context, pr *model.ProvisionRun) (*model.ProvisionRun, error)
	GetProvisionRun(ctx context.Context, id int64) (*model.ProvisionRun, error)
	SetProvisionRunStatus(ctx context.Context, id int64, status, errMsg string, finished bool) error
	AppendProvisionRunLog(ctx context.Context, id int64, chunk string) error
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

	mu      sync.Mutex // serializes runs
	running bool
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

// Validate checks the request shape before any resources are allocated.
func (r *Request) Validate() error {
	if !hostRe.MatchString(r.Host) {
		return fmt.Errorf("invalid host %q", r.Host)
	}
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
	if r.WinRMUser == "" {
		r.WinRMUser = "Administrator"
	}
	if r.WinRMPassword == "" {
		return fmt.Errorf("winrm_password is required")
	}
	if r.NodeName == "" {
		return fmt.Errorf("node_name is required")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(r.NodeName) {
		return fmt.Errorf("node_name %q contains invalid characters", r.NodeName)
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
		OptionsJSON: fmt.Sprintf(`{"install_toolchain":%v,"mount_nfs":%v,"push_bin":%v}`,
			req.InstallToolchain, req.MountNFS, req.PushBin),
	})
	if err != nil {
		return nil, err
	}
	go e.run(pr.ID, req)
	return pr, nil
}

// run executes one provisioning attempt end to end.
func (e *Engine) run(id int64, req Request) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	// Serialize runs: queue behind any in-flight run.
	e.mu.Lock()
	defer e.mu.Unlock()

	bg := context.Background()
	_ = e.Store.SetProvisionRunStatus(bg, id, "running", "", false)

	err := e.execute(ctx, bg, id, req)
	if err != nil {
		_ = e.Store.AppendProvisionRunLog(bg, id, "\n[controller] FAILED: "+err.Error()+"\n")
		_ = e.Store.SetProvisionRunStatus(bg, id, "failed", err.Error(), true)
		return
	}
	_ = e.Store.AppendProvisionRunLog(bg, id, "\n[controller] SUCCESS: node provisioned\n")
	_ = e.Store.SetProvisionRunStatus(bg, id, "success", "", true)
}

// execute performs staging, runs ansible-playbook, and streams the output.
func (e *Engine) execute(ctx, bg context.Context, id int64, req Request) error {
	settings, err := e.Store.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if settings == nil {
		return fmt.Errorf("no settings configured — open the Settings page first")
	}
	if strings.TrimSpace(settings.ControllerURL) == "" {
		return fmt.Errorf("settings.controller_url is empty — set it on the Settings page (nodes must be able to reach this URL)")
	}

	dir, err := os.MkdirTemp("", "provision-*")
	if err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// 1. Stage payloads from the update store.
	agentOK, err := e.Payloads.StageAgentBinary(filepath.Join(dir, "files", "agent", "encode-agent.exe"))
	if err != nil {
		return fmt.Errorf("stage agent binary: %w", err)
	}
	if !agentOK {
		return fmt.Errorf("no agent binary published — publish one on Settings → Push to nodes first")
	}
	libOK, err := e.Payloads.StageLib(filepath.Join(dir, "files", "agent", "EncodeLib.ps1"))
	if err != nil {
		return fmt.Errorf("stage EncodeLib: %w", err)
	}
	binOK := false
	if req.PushBin {
		binOK, err = e.Payloads.StageBinZip(filepath.Join(dir, "files", "bin-package.zip"))
		if err != nil {
			return fmt.Errorf("stage bin package: %w", err)
		}
		if !binOK {
			return fmt.Errorf("push_bin requested but no bin package is published")
		}
	}

	// 2. Pairing code — single-use bootstrap, plaintext only in vars.yml.
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
	vars := buildVars(settings, req, code, libOK, binOK)
	if err := os.WriteFile(filepath.Join(dir, "vars.yml"), []byte(vars), 0o600); err != nil {
		return err
	}

	// 4. Run ansible-playbook, streaming output into the run log.
	playbook := filepath.Join(e.PlaybookDir, "provision.yml")
	if _, err := os.Stat(playbook); err != nil {
		return fmt.Errorf("playbook missing: %w", err)
	}
	ansibleBin := e.AnsibleBin
	if ansibleBin == "" {
		ansibleBin = "ansible-playbook"
	}
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
	return streamCommand(ctx, bg, e.Store, id, cmd, e.logCap())
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
func streamCommand(ctx, bg context.Context, st Store, id int64, cmd *exec.Cmd, capBytes int) error {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ansible-playbook: %w", err)
	}

	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	flush := func() {
		mu.Lock()
		chunk := buf.String()
		buf.Reset()
		mu.Unlock()
		if chunk != "" {
			_ = st.AppendProvisionRunLog(bg, id, chunk)
		}
	}
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(logFlushInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				flush()
			}
		}
	}()

	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 64*1024), 512*1024)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		mu.Lock()
		// capBytes bounds the per-run log size: drop the oldest lines when
		// the cap is exceeded (keep the tail — that is where the failure is).
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
	pw.Close()
	close(done)
	flush()

	err := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("provisioning timed out after %s", runTimeout)
	}
	if err != nil {
		return fmt.Errorf("ansible-playbook exited with an error (see log)")
	}
	return nil
}

// buildVars renders the ansible extra-vars file from live settings + the
// request. The WinRM password and pairing code are the only secrets and
// live nowhere else.
func buildVars(s *model.Settings, req Request, pairingCode string, libOK, binOK bool) string {
	nfsScripts := ""
	nfsRelease := ""
	if req.MountNFS && strings.TrimSpace(s.NFSServer) != "" {
		server := strings.TrimSpace(s.NFSServer)
		if sp := strings.TrimSpace(s.ScriptsShare); sp != "" {
			nfsScripts = server + ":" + sp
		}
		if rp := strings.TrimSpace(s.ReleaseShare); rp != "" {
			nfsRelease = server + ":" + rp
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
	fmt.Fprintf(&b, "encode_mount_nfs: %v\n", req.MountNFS && nfsScripts != "")
	fmt.Fprintf(&b, "encode_install_toolchain: %v\n", req.InstallToolchain)
	fmt.Fprintf(&b, "encode_push_bin: %v\n", binOK)
	fmt.Fprintf(&b, "encode_has_lib: %v\n", libOK)
	fmt.Fprintf(&b, "encode_pairing_code: %s\n", yamlQuote(pairingCode))
	fmt.Fprintf(&b, "ansible_user: %s\n", yamlQuote(req.WinRMUser))
	fmt.Fprintf(&b, "ansible_password: %s\n", yamlQuote(req.WinRMPassword))
	fmt.Fprintf(&b, "ansible_connection: winrm\n")
	fmt.Fprintf(&b, "ansible_winrm_transport: ntlm\n")
	fmt.Fprintf(&b, "ansible_winrm_server_cert_validation: ignore\n")
	fmt.Fprintf(&b, "ansible_winrm_read_timeout_sec: 120\n")
	fmt.Fprintf(&b, "ansible_winrm_operation_timeout_sec: 130\n")
	return b.String()
}

// yamlQuote single-quotes a scalar, escaping embedded single quotes. Keeps
// arbitrary settings values (paths with backslashes, etc.) YAML-safe without
// a full marshaler dependency.
func yamlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
