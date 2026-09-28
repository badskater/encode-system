// Package model defines the core domain types shared across the controller
// and (via the API) the agents and UI.
package model

import "time"

// JobStatus is the lifecycle state of a job.
type JobStatus string

const (
	JobPending   JobStatus = "pending"   // created by scanner, waiting for a node
	JobAssigned  JobStatus = "assigned"  // handed to a node, not yet running
	JobRunning   JobStatus = "running"   // agent executing steps
	JobDone      JobStatus = "done"      // completed successfully
	JobFailed    JobStatus = "failed"    // terminal failure
	JobCancelled JobStatus = "cancelled" // cancelled by an operator
)

// Terminal reports whether the status ends the job's lifecycle.
func (s JobStatus) Terminal() bool {
	return s == JobDone || s == JobFailed || s == JobCancelled
}

// NodeStatus is the controller's view of a node's liveness.
type NodeStatus string

const (
	NodeIdle    NodeStatus = "idle"
	NodeBusy    NodeStatus = "busy"
	NodeOffline NodeStatus = "offline"
	NodeReboot  NodeStatus = "reboot_pending" // reboot instruction issued, waiting
)

// StepType identifies a pipeline step template in a flow.
type StepType string

const (
	StepSourceRename StepType = "source_rename"
	StepDGIndex      StepType = "dgindex"
	StepAudio        StepType = "audio"
	StepEncode       StepType = "encode"
	StepMux          StepType = "mux"
	StepReleaseCopy  StepType = "release_copy"
	StepKeyframes    StepType = "keyframes"
)

// Step is one entry in a flow: a reference to a step template (by key)
// plus its parameters. Type mirrors the template key and stays for
// backward compatibility with the built-in step constants.
type Step struct {
	Type   StepType          `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

// TemplateKey returns the step's template reference (defaults to its type).
func (s Step) TemplateKey() string { return string(s.Type) }

// Flow is a named, ordered list of steps rendered into a job script.
// Exactly one flow may be the default (used when a series has no flow set).
type Flow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Steps     []Step `json:"steps"`
	IsDefault bool   `json:"is_default"`
	// MaxRetries is the per-flow automatic retry budget: a failed job whose
	// retry_count is below this is silently re-queued with backoff instead of
	// alerting. Zero = retry OFF (the default for flows created before D1).
	// Stored in options_json alongside RetryBackoffMinutes.
	MaxRetries int `json:"max_retries"`
	// RetryBackoffMinutes is the delay before a re-queued job becomes
	// assignable again. Applied only when MaxRetries > 0; a zero value when
	// retries are enabled defaults to a sensible minimum at the decision
	// site so a retry is never instant. Stored in options_json.
	RetryBackoffMinutes int       `json:"retry_backoff_minutes"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// User is a management-plane account (login-based, replaces the static
// admin bearer token).
type User struct {
	ID           int64      `json:"id"`
	Username     string     `json:"username"`
	Role         string     `json:"role"` // "admin"
	PasswordHash string     `json:"-"`    // never serialized
	CreatedAt    *time.Time `json:"created_at,omitempty"`
}

// ProvisionRun is one controller-driven Ansible provisioning attempt of a
// Windows node. The WinRM password is NEVER stored: it lives only in the
// temporary vars file for the duration of the run.
type ProvisionRun struct {
	ID          int64      `json:"id"`
	Host        string     `json:"host"`   // WinRM target (ip or hostname)
	Port        int        `json:"port"`   // WinRM port (default 5985)
	Scheme      string     `json:"scheme"` // http | https
	WinRMUser   string     `json:"winrm_user"`
	NodeName    string     `json:"node_name"`     // desired agent/node name
	Status      string     `json:"status"`        // queued|running|success|failed
	OptionsJSON string     `json:"-"`             // run options snapshot (no secrets)
	Log         string     `json:"log,omitempty"` // full ansible output (single-run GET only)
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// ShareKind is the transport backing a share mount.
type ShareKind string

const (
	ShareNFS  ShareKind = "nfs"
	ShareSMB  ShareKind = "smb"
	ShareS3   ShareKind = "s3"
	ShareNone ShareKind = ""
)

// ShareRole is what the share provides to the pipeline.
type ShareRole string

const (
	ShareRoleScripts ShareRole = "scripts" // job scripts + sources
	ShareRoleRelease ShareRole = "release" // finished outputs
)

// Share is a storage source mounted (nfs/smb) or accessed (s3) by nodes.
// Replaces the flat NFS Settings fields: one row per role+kind, so a farm
// can mix transports (SMB on Windows nodes, S3 for distributed pulls).
// Password/secret are encrypted at rest (AES-GCM, key file beside the DB)
// and never serialized to API responses.
type Share struct {
	ID     int64     `json:"id"`
	Name   string    `json:"name"`
	Kind   ShareKind `json:"kind"` // nfs | smb | s3
	Role   ShareRole `json:"role"` // scripts | release
	Server string    `json:"server"`
	Path   string    `json:"path"` // export path (nfs), share name (smb), bucket (s3)
	Port   int       `json:"port,omitempty"`
	// Credentials: smb user/password, s3 access/secret key. Empty for nfs.
	Username string `json:"username,omitempty"`
	Password string `json:"-"` // write-only over the API; never marshaled out
	// S3 extras.
	Region   string `json:"region,omitempty"`
	UseTLS   bool   `json:"use_tls,omitempty"`
	Endpoint string `json:"endpoint,omitempty"` // s3: full URL override (minio/RGW)
	Enabled  bool   `json:"enabled"`
	// MountPath is where provisioning mounts it on the node (nfs/smb).
	// S3 shares have no mount — the agent pulls per job.
	MountPath string    `json:"mount_path,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Settings is the operator-editable runtime configuration (NFS shares,
// controller roots, node path mapping, scan cadence, release naming). Stored
// in the database as a JSON blob in a single-row table; environment variables
// only seed the first boot.
type Settings struct {
	// Controller URL as seen by the NODES (provisioned agents connect here;
	// the container's own hostname is usually meaningless outside Docker).
	ControllerURL string `json:"controller_url"`
	// NFS share description (informational + deployment guidance: the
	// actual mounts are compose volumes on the Docker host).
	NFSServer    string `json:"nfs_server"`
	ScriptsShare string `json:"scripts_share"` // export path, e.g. /mnt/user/scripts
	ReleaseShare string `json:"release_share"` // export path, e.g. /mnt/user/ReleaseFolders
	// Controller-side roots (where the shares are mounted in the container).
	ScriptsRoot string `json:"scripts_root"`
	ReleaseRoot string `json:"release_root"`
	// Remote path mapping: node-side locations used when rendering job
	// scripts ($Job.BinDir / ScriptsDir / ReleaseDir).
	NodeBinDir     string `json:"node_bin_dir"`
	NodeScriptsDir string `json:"node_scripts_dir"`
	NodeReleaseDir string `json:"node_release_dir"`
	// Behavior.
	ScanIntervalSeconds int    `json:"scan_interval_seconds"`
	TasksBeforeReboot   int    `json:"tasks_before_reboot"`
	Group               string `json:"group"`
	Tag                 string `json:"tag"`
	// DiscordWebhook is the live-editable Discord webhook used by both the
	// job-outcome alerts and the discord_notify flow step (as the fallback
	// when a flow omits its own webhook param). Empty = notifications off.
	DiscordWebhook string `json:"discord_webhook"`
	// DiskAlertGB is the free-disk threshold (GB) on the drive the agent
	// measures. A heartbeat reporting less free space (1) fires a Discord
	// alert with a per-node cooldown and (2) soft-drains the node: no new
	// job is assigned until it recovers above the threshold. 0 = disabled
	// (default) — no alert, no drain, old behavior.
	DiskAlertGB int64 `json:"disk_alert_gb"`
	// DrainMode stops the queue from assigning new jobs to nodes — used to
	// safely drain the farm for maintenance without cancelling in-flight
	// encodes. Persisted in the settings JSON blob (not a SQL column).
	DrainMode bool `json:"drain_mode"`
	// NotifyDigest collapses per-job outcome alerts into a periodic digest
	// when true; false keeps the immediate per-job alert behavior. Persisted
	// in the settings JSON blob (not a SQL column).
	NotifyDigest bool `json:"notify_digest"`

	// JobRetentionDays bounds job history: terminal jobs (done/failed/
	// cancelled) finished more than this many days ago are pruned by the
	// hourly retention loop. 0 = keep forever (the default, and the zero
	// value old settings rows unmarshal to). Validated 0 or 1-3650.
	JobRetentionDays int `json:"job_retention_days"`

	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// Session is an issued management session (token stored hashed at rest).
type Session struct {
	TokenHash string     `json:"-"`
	UserID    int64      `json:"user_id"`
	Username  string     `json:"username"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Node is one Windows encode machine.
type Node struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"`
	Enabled   bool       `json:"enabled"`
	Status    NodeStatus `json:"status"`
	// Group is a free-form routing label. Jobs whose series has a matching
	// node_group route to this node; an empty group is a wildcard that
	// accepts any job. Set via PATCH /api/nodes/{id} {group}.
	Group string `json:"group"`
	// MaxConcurrentJobs is how many jobs this node may run at once
	// (default 1 = the historical one-job-per-node rule). Light steps
	// (audio, mux) benefit from overlap; heavy x265 encodes usually want 1.
	// Clamped 1-8 by SetNodeMaxConcurrent.
	MaxConcurrentJobs int `json:"max_concurrent_jobs"`
	// ActiveJobs is a transient count of assigned/running jobs, filled by
	// ListNodes for the UI ("1/2 slots" display). Never persisted.
	ActiveJobs     int    `json:"active_jobs"`
	AgentVersion   string `json:"agent_version"`
	LibVersion     int64  `json:"lib_version"`
	BinVersion     int64  `json:"bin_version"` // bin package version on node (0 = none)
	TasksSinceBoot int    `json:"tasks_since_boot"`
	RebootPending  bool   `json:"reboot_pending"`
	// RebootIssuedAtTasks snapshots the counter when the reboot instruction
	// was issued. A heartbeat reporting fewer tasks proves the node rebooted.
	RebootIssuedAtTasks int `json:"-"`
	// RebootIssuedAt records when the instruction was issued; after a grace
	// period the flag expires so a node cannot be locked out forever.
	RebootIssuedAt *time.Time `json:"-"`
	// Metrics is the last reported resource sample from a heartbeat, kept
	// in-memory only (NOT persisted on the nodes table — the node_metrics
	// ring table holds history). This is the live in-process heartbeat
	// sample; the restart-safe last-known sample is served from the DB via
	// Store.LatestNodeMetric on the ListNodes path (see last_metrics there).
	// nil for old agents that do not report metrics yet.
	Metrics   *NodeMetrics `json:"metrics,omitempty"`
	LastSeen  *time.Time   `json:"last_seen,omitempty"`
	LastError string       `json:"last_error,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// Job is one episode encode assignment.
type Job struct {
	ID         int64      `json:"id"`
	Series     string     `json:"series"`
	Episode    string     `json:"episode"`     // e.g. "01"
	EpisodeDir string     `json:"episode_dir"` // path relative to scripts share, e.g. "Series Name/Ep 01"
	ScriptType string     `json:"script_type"` // "avs" or "vpy"
	ScriptFile string     `json:"script_file"` // detected filter script, e.g. "1080.vpy"
	FlowID     int64      `json:"flow_id"`
	Status     JobStatus  `json:"status"`
	NodeID     int64      `json:"node_id,omitempty"`
	Step       string     `json:"step,omitempty"`
	Progress   float64    `json:"progress,omitempty"`
	ExitCode   int        `json:"exit_code"`
	Error      string     `json:"error,omitempty"`
	LogTail    string     `json:"log_tail,omitempty"`
	Outputs    []string   `json:"outputs,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// FullLog is the complete captured stdout/stderr of the job (unlike
	// LogTail which is a bounded tail). Kept on the jobs row so the UI can
	// show the full run without re-reading agent files. Empty = no full
	// log captured yet (old jobs pre-v2 carry '').
	FullLog string `json:"full_log,omitempty"`
	// StepTimings records per-step start time and wall-clock duration for
	// the observability dashboard. Unmarshaled from the step_timings_json
	// column; nil/empty when the agent has not reported any timings.
	StepTimings []StepTiming `json:"step_timings,omitempty"`
	// Priority is the queue dispatch weight (higher = dispatched sooner).
	// Defaults to 0 (FIFO order) for jobs created before the priority field.
	Priority int `json:"priority,omitempty"`
	// RetryCount is how many times this job has been re-queued after
	// failure. The queue uses it to apply backoff via NextRetryAt.
	RetryCount int `json:"retry_count,omitempty"`
	// NextRetryAt gates re-dispatch of a failed-then-retried job: the
	// queue will not pick it up until this time. nil = immediately ready.
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`

	// Metrics holds ENCODE_METRIC key=value pairs reported by the job
	// script (vmaf, output_bitrate_kbps, duration_sec, sizes…). Unmarshaled
	// from metrics_json; nil/empty when the script reported none.
	Metrics map[string]float64 `json:"metrics,omitempty"`

	// LastFailedNodeID is the node that most recently failed this job (0 =
	// never failed). The dispatcher steers re-queued retries to a different
	// node when one is available — a node-local fault (disk, GPU, corrupt
	// toolchain) would otherwise re-fail every retry on the same box.
	// Cleared on success; preserved across retries (that's the point).
	LastFailedNodeID int64 `json:"last_failed_node_id,omitempty"`
}

// StepTiming is one per-step wall-clock sample on a job. The controller
// populates it from agent heartbeat/report data to render the observability
// timeline. StartedAt is the step's start; DurationSec is elapsed seconds.
type StepTiming struct {
	Step        string    `json:"step"`
	StartedAt   time.Time `json:"started_at"`
	DurationSec float64   `json:"duration_sec"`
}

// NodeMetrics is a point-in-time resource sample for a node, reported by the
// agent in its heartbeat and persisted into the node_metrics ring table for
// history. GPU fields use -1 to mean "no GPU reported" (a headless node or
// one whose agent predates GPU telemetry); callers must guard on <0.
type NodeMetrics struct {
	CPUPct       float64 `json:"cpu_pct"`
	MemUsedMB    int64   `json:"mem_used_mb"`
	MemTotalMB   int64   `json:"mem_total_mb"`
	DiskFreeGB   int64   `json:"disk_free_gb"`
	GPUUtil      int     `json:"gpu_util"`        // -1 = no GPU reported
	GPUTemp      int     `json:"gpu_temp"`        // -1 = no GPU reported
	GPUMemUsedMB int     `json:"gpu_mem_used_mb"` // -1 = no GPU reported
	EncodeFPS    float64 `json:"encode_fps"`
}

// NodeMetricSample is one persisted row from the node_metrics ring table, as
// served to the dashboard via GET /api/nodes/{id}/metrics. It mirrors the
// persisted columns; Ts is the sample timestamp in UTC. GPU fields use -1 to
// mean "no GPU reported" (same convention as NodeMetrics).
type NodeMetricSample struct {
	Ts           time.Time `json:"ts"`
	CPUPct       float64   `json:"cpu_pct"`
	MemUsedMB    int64     `json:"mem_used_mb"`
	MemTotalMB   int64     `json:"mem_total_mb"`
	DiskFreeGB   int64     `json:"disk_free_gb"`
	GPUUtil      int       `json:"gpu_util"`
	GPUTemp      int       `json:"gpu_temp"`
	GPUMemUsedMB int       `json:"gpu_mem_used_mb"`
	EncodeFPS    float64   `json:"encode_fps"`
}

// Series is a registered show/folder on the scripts share with its own flow
// assignment and enable state. Episodes of a series may run on any enabled
// node (the queue distributes one job per idle node automatically).
type Series struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`    // matches the share folder name exactly
	FlowID  int64  `json:"flow_id"` // 0 = use the default flow
	Tag     string `json:"tag"`     // quality tag override; "" = global settings tag
	Enabled bool   `json:"enabled"`
	// Paused is a stronger hold than Enabled=false: the scanner skips the
	// series AND already-queued pending jobs do not dispatch until it is
	// lifted. Enabled=false only stops new job creation; queued jobs still
	// run. Pausing is for "stop the presses" without losing queue position.
	Paused bool `json:"paused"`
	// NodeGroup is a free-form routing label. When non-empty, the series'
	// jobs only dispatch to nodes whose Group matches; nodes with an empty
	// Group (wildcard) still accept them. Set via PATCH /api/series/{id}.
	NodeGroup string `json:"node_group"`
	// Notify controls whether job outcomes for this series emit Discord
	// alerts. Defaults true (matches pre-v2 behavior where every job
	// alerted) so a silent series is an opt-in, not an opt-out.
	Notify bool `json:"notify"`
	// WebhookURL overrides the global Discord webhook for this series'
	// job-outcome alerts (per-series channels). Empty = use the global
	// webhook. Only honored on the direct-alert path; digest mode batches
	// everything into the global channel by design.
	WebhookURL string    `json:"webhook_url,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// StepTemplate is one controllable pipeline section: metadata plus its own
// PowerShell function. Built-ins mirror EncodeLib.ps1; custom templates add
// new steps the renderer embeds into the final flow script.
type StepTemplate struct {
	ID          int64      `json:"id"`
	Key         string     `json:"key"` // unique, e.g. "dgindex" or "my_cleanup"
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Params      []ParamDef `json:"params"`
	PowerShell  string     `json:"powershell"` // full function source
	Builtin     bool       `json:"builtin"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ParamDef declares one editable parameter of a step template. Type drives
// the flow-builder widget ("text" default, "bool", "number"); Default is the
// runtime value when a flow omits the param (the script applies it).
type ParamDef struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
	Default     string `json:"default,omitempty"`
	Type        string `json:"type,omitempty"`
}

// PairingCode is a one-shot code an agent presents to register itself.
type PairingCode struct {
	ID        int64     `json:"id"`
	CodeHash  string    `json:"-"`
	NameHint  string    `json:"name_hint"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedBy    int64     `json:"used_by,omitempty"` // node id once consumed
	CreatedAt time.Time `json:"created_at"`
}

// HeartbeatJobReport is one in-flight job's live state within a heartbeat.
// A concurrent-capable agent reports one entry per running job; the
// controller updates each job's status/progress and fans it to SSE.
type HeartbeatJobReport struct {
	JobID        int64   `json:"job_id"`
	JobStatus    string  `json:"job_status,omitempty"`
	Step         string  `json:"step,omitempty"`
	StepProgress float64 `json:"step_progress,omitempty"`
	LogTail      string  `json:"log_tail,omitempty"`
}

// Heartbeat is the periodic agent status report.
type Heartbeat struct {
	Node           string `json:"node"`
	AgentVersion   string `json:"agent_version"`
	LibVersion     int64  `json:"lib_version"`
	BinVersion     int64  `json:"bin_version"` // bin package version on disk (0 = none)
	Syncing        bool   `json:"syncing"`     // update sync in flight: treat node as busy
	TasksSinceBoot int    `json:"tasks_since_boot"`
	// Legacy single-job fields, still sent by concurrency-capable agents
	// for their FIRST active job so an old controller keeps working during
	// a rolling upgrade. Prefer Jobs when non-empty.
	JobID        int64   `json:"job_id,omitempty"`
	JobStatus    string  `json:"job_status,omitempty"`
	Step         string  `json:"step,omitempty"`
	StepProgress float64 `json:"step_progress,omitempty"`
	LogTail      string  `json:"log_tail,omitempty"`
	// Jobs carries every in-flight job (per-node concurrency). Empty for
	// old agents; the controller falls back to the legacy single fields.
	Jobs []HeartbeatJobReport `json:"jobs,omitempty"`
	// Metrics is the agent's latest resource sample. nil for old agents
	// that do not report metrics — callers must nil-check before use.
	Metrics *NodeMetrics `json:"metrics,omitempty"`
}

// jobReports normalizes the heartbeat into a per-job list: the Jobs array
// when present (new agents), else the legacy single-job fields (old agents).
// The controller and tests share one code path via this method.
func (h *Heartbeat) JobReports() []HeartbeatJobReport {
	if len(h.Jobs) > 0 {
		return h.Jobs
	}
	if h.JobID > 0 {
		return []HeartbeatJobReport{{
			JobID: h.JobID, JobStatus: h.JobStatus, Step: h.Step,
			StepProgress: h.StepProgress, LogTail: h.LogTail,
		}}
	}
	return nil
}

// JobPayload is what the controller hands to an agent to run a job.
type JobPayload struct {
	ID     int64             `json:"id"`
	Script string            `json:"script"` // rendered PowerShell
	Vars   map[string]string `json:"vars"`   // job variables for logging/context
	Flow   string            `json:"flow"`   // flow name, informational
	// S3 carries per-job object-store transfers when a scripts/release
	// role is backed by an s3 share instead of a mount: the agent
	// downloads every Transfer.Download prefix into its local dir BEFORE
	// running the script and uploads each Upload dir AFTER a successful
	// run. Nil for mount-backed jobs (the common case).
	S3 *S3Transfer `json:"s3,omitempty"`
}

// S3Transfer is the per-job object-store work order. Credentials are
// short-lived in the sense that they ride one dispatch reply over the
// node-authenticated channel; the controller never logs the struct.
type S3Transfer struct {
	Endpoint  string       `json:"endpoint"` // host:port (no scheme)
	Region    string       `json:"region,omitempty"`
	AccessKey string       `json:"access_key"`
	SecretKey string       `json:"secret_key"`
	UseTLS    bool         `json:"use_tls,omitempty"`
	Downloads []S3Download `json:"downloads,omitempty"`
	Uploads   []S3Upload   `json:"uploads,omitempty"`
}

// S3Object is one listed object (key + size) from a bucket prefix scan.
type S3Object struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

// S3Download pulls bucket/prefix/** into LocalDir (created if missing)
// before the job script runs.
type S3Download struct {
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"` // e.g. "4k-test/Ep 02" (no leading slash)
	LocalDir string `json:"local_dir"`
}

// S3Upload pushes every file under LocalDir to bucket/prefix after a
// successful run (recursive, relative paths preserved).
type S3Upload struct {
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
	LocalDir string `json:"local_dir"`
}

// APIToken is a scoped token for external automation (Sonarr-style
// triggers, dashboards, scripts). Only TokenHash is stored; the plaintext
// token is shown once at creation. Scope "admin" grants full management
// API access; "read" is restricted to GET endpoints.
type APIToken struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	TokenHash  string     `json:"-"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
}

// Token scopes.
const (
	TokenScopeAdmin = "admin"
	TokenScopeRead  = "read"
)

// AuditEvent is one row of the audit log: who did what to which object.
// Written fire-and-forget by mutating handlers; Detail carries a small
// JSON snippet of the change (never secrets or full bodies).
type AuditEvent struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`  // session username or "api-token:<name>"
	Action string    `json:"action"` // e.g. "settings.update", "node.delete"
	Object string    `json:"object"` // e.g. "node:3", "series:5", "settings"
	Detail string    `json:"detail"` // small JSON snippet, "" when nothing to add
}

// UpdateManifest describes the agent/lib/bin versions the controller wants
// deployed. Agents compare each field against what they run and sync what
// differs (lib first, then the bin folder, then the agent binary itself).
type UpdateManifest struct {
	AgentVersion string `json:"agent_version"`
	AgentSHA256  string `json:"agent_sha256"`
	// Previous agent release kept for rollback: PublishAgent rotates the
	// outgoing payload into encode-agent.exe.prev and records its identity
	// here. Empty until a second publish has happened. Agents never read
	// these — rollback promotes prev to current server-side, and nodes
	// self-downgrade because AgentVersion then differs from what they run.
	PrevAgentVersion string `json:"prev_agent_version,omitempty"`
	PrevAgentSHA256  string `json:"prev_agent_sha256,omitempty"`
	LibVersion       int64  `json:"lib_version"`
	LibSHA256        string `json:"lib_sha256"`
	// Bin folder package: a zip of the node tools dir (C:\bin). Version is a
	// publish counter; agents extract it over their bin dir when it differs.
	BinVersion int64  `json:"bin_version"`
	BinSHA256  string `json:"bin_sha256"`
	BinSize    int64  `json:"bin_size,omitempty"`
}

// HeartbeatReply is the controller's instruction channel to the agent.
type HeartbeatReply struct {
	Instruction string `json:"instruction"` // none | job | jobs | reboot | update
	// Job carries a single assignment. Kept for old agents: when the
	// controller assigns to an old agent (no Jobs reported) only Job is
	// set. New agents prefer the Jobs array.
	Job *JobPayload `json:"job,omitempty"`
	// Jobs carries one or more assignments for concurrency-capable agents
	// (the agent's reported free slots). Job is ALSO set to Jobs[0] so an
	// old agent receiving this reply still runs exactly one job.
	Jobs        []*JobPayload   `json:"jobs,omitempty"`
	RebootDelay int             `json:"reboot_delay_seconds,omitempty"`
	Update      *UpdateManifest `json:"update,omitempty"`
}

// ---------- Fleet stats (Phase E) ----------

// Stats is the aggregate job-history snapshot served by GET /api/stats.
// Every field is derived in SQL from the existing jobs table + joins to
// nodes/flows — no new storage. The range window (finished_at >= now-range)
// scopes the aggregates; RangeDays echoes the resolved window so the UI can
// label the view. An empty/never-used fleet returns a zeroed shape (not an
// error) so the Stats page renders cleanly on a fresh install.
type Stats struct {
	RangeDays      int            `json:"range_days"`       // 1|7|30|0(all)
	Totals         StatsTotals    `json:"totals"`           // done/failed/cancelled counts + avg duration
	PerNode        []StatsNodeRow `json:"per_node"`         // per-node throughput/duration (only nodes with rows in range)
	PerFlow        []StatsFlowRow `json:"per_flow"`         // per-flow throughput/duration
	FailuresByStep []StatsStepRow `json:"failures_by_step"` // top-10 steps where jobs died
	PerDay         []StatsDayRow  `json:"per_day"`          // day buckets for throughput trend
}

// StatsTotals aggregates the fleet-wide terminal counts and the average
// finished-job duration within the range window. AvgDurationSec is 0 when
// no finished jobs have both started_at and finished_at (the SQL AVG returns
// NULL over zero rows — coerced to 0).
type StatsTotals struct {
	Done           int     `json:"done"`
	Failed         int     `json:"failed"`
	Cancelled      int     `json:"cancelled"`
	AvgDurationSec float64 `json:"avg_duration_sec"`
}

// StatsNodeRow is one per-node aggregate row: the node's display name (LEFT
// JOIN nodes), counts of done/failed jobs it completed in range, and its
// average finished-job duration. Only nodes that have ≥1 finished job in
// range appear (zero-row nodes are skipped — simpler, and the Nodes page
// already lists the full fleet).
type StatsNodeRow struct {
	NodeID         int64   `json:"node_id"`
	Name           string  `json:"name"`
	Done           int     `json:"done"`
	Failed         int     `json:"failed"`
	AvgDurationSec float64 `json:"avg_duration_sec"`
}

// StatsFlowRow is one per-flow aggregate row (LEFT JOIN flows for the name).
type StatsFlowRow struct {
	FlowID         int64   `json:"flow_id"`
	Name           string  `json:"name"`
	Done           int     `json:"done"`
	Failed         int     `json:"failed"`
	AvgDurationSec float64 `json:"avg_duration_sec"`
}

// StatsStepRow is one row of failures-by-step attribution: the step column
// (the last/current step the job reported) and how many jobs failed there.
// Ordered by count DESC, capped at 10.
type StatsStepRow struct {
	Step  string `json:"step"`
	Count int    `json:"count"`
}

// StatsDayRow is one day bucket of completed jobs for the throughput trend.
// Date is the "YYYY-MM-DD" day slice of finished_at.
type StatsDayRow struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}
