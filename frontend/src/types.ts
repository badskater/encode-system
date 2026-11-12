// Shared API types mirroring the Go backend's model package.

export type JobStatus = 'pending' | 'assigned' | 'running' | 'done' | 'failed' | 'cancelled';
export type NodeStatus = 'idle' | 'busy' | 'offline' | 'reboot_pending';
export type StepType =
  | 'source_rename'
  | 'dgindex'
  | 'audio'
  | 'encode'
  | 'mux'
  | 'release_copy'
  | 'keyframes';

export interface Step {
  type: string;
  params?: Record<string, string>;
  // Per-step wall-clock limit enforced by the agent (minutes). 0/absent =
  // no per-step limit (the flow's job timeout, if any, still applies).
  timeout_minutes?: number;
}

export interface Flow {
  id: number;
  name: string;
  steps: Step[];
  is_default: boolean;
  created_at: string;
  updated_at: string;
  // Retry policy (Phase D2). max_retries=0 (or absent) means no auto-retry.
  // retry_backoff_minutes is the delay between retries; only meaningful when
  // max_retries > 0.
  max_retries?: number;
  retry_backoff_minutes?: number;
  // Whole-job wall-clock limit enforced by the agent (minutes). The agent
  // kills the job's process tree and reports a failure when it expires.
  // 0/absent = no timeout. Valid range on save: 0-10080.
  job_timeout_minutes?: number;
}

export interface Series {
  id: number;
  name: string;
  flow_id: number; // 0 = default flow
  tag: string; // quality tag override; "" = global settings tag
  enabled: boolean;
  jobs?: number;
  // Phase F1: per-series encode progress. Done counts distinct episodes
  // with a successful job; Failed counts episodes that never succeeded
  // (an episode that eventually succeeded is done, not failed); Active
  // counts episodes with an in-flight job; Total is the best available
  // denominator (scaffolded folders on disk, or distinct dirs from jobs).
  episodes_done?: number;
  episodes_failed?: number;
  episodes_active?: number;
  episodes_total?: number;
  // Phase F2: per-series notify mute. When false the controller skips this
  // series' Discord job-outcome alerts (done/failed); other series still
  // notify. Defaults to true server-side for series created before the flag.
  notify?: boolean;
  // Per-series pause: stronger than enabled=false. Paused series skip the
  // scanner AND their already-queued pending jobs hold at dispatch until
  // unpause; queue position is preserved. Absent/false = running.
  paused?: boolean;
  // Per-series Discord webhook override for job-outcome alerts. Empty or
  // absent = use the global webhook from Settings.
  webhook_url?: string;
  // Routing label: non-empty restricts this series' jobs to nodes with a
  // matching group (wildcard nodes still accept them). '' = any node.
  node_group?: string;
  created_at: string;
  updated_at: string;
}

// Response of POST /api/series (create series with folder scaffolding).
export interface CreateSeriesResponse {
  series: Series;
  scripts_folder: string;
  release_folder: string;
  episode_folders: string[];
}

export interface ParamDef {
  key: string;
  label: string;
  placeholder?: string;
  /** Runtime default applied by the step script when the flow omits the value. */
  default?: string;
  /** Widget type in the flow builder. Unknown values degrade to a text input. */
  type?: ParamType;
}

export type ParamType = 'text' | 'number' | 'bool';

// Settings: NFS shares, controller roots, remote path mapping, behavior.
export interface Settings {
  controller_url?: string;
  nfs_server?: string;
  scripts_share?: string;
  release_share?: string;
  scripts_root: string;
  release_root: string;
  node_bin_dir: string;
  node_scripts_dir: string;
  node_release_dir: string;
  // Per-platform node path overrides for Linux nodes. When non-empty and the
  // target node's platform is "linux", the rendered job script uses these
  // POSIX paths instead of the Windows values above. Empty = fall back to the
  // Windows value (back-compat). S3 staging still wins over both.
  linux_bin_dir?: string;
  linux_scripts_dir?: string;
  linux_release_dir?: string;
  scan_interval_seconds: number;
  tasks_before_reboot: number;
  group: string;
  tag: string;
  discord_webhook: string; // blank = Discord notifications off
  // Drain mode (Phase D3): when true the backend pauses ALL job assignment
  // fleet-wide (running jobs finish; nothing new dispatches). Toggled from
  // the Settings page; live — no controller restart needed.
  drain_mode?: boolean;
  // Phase F2: hourly digest. When true the controller batches job-outcome
  // alerts into one Discord summary per hour instead of one message per job.
  // Round-trips via PUT /api/settings; absent/unchecked = per-job alerts.
  notify_digest?: boolean;
  // Job history retention: terminal jobs finished more than this many days
  // ago are pruned hourly by the controller. 0 (default) = keep forever.
  // Valid range on save: 0 or 1-3650.
  job_retention_days?: number;
  // Free-disk alert threshold (GB) measured on the agent's work drive.
  // A node below it fires a Discord alert (1/hour per node) and takes no
  // new jobs until it recovers. 0/absent = disabled.
  disk_alert_gb?: number;
  // Node status alerts: when true the controller posts a Discord alert
  // when a node transitions between online/offline/busy states. Off by
  // default — only meaningful when discord_webhook is also set.
  notify_node_status?: boolean;
  // Stall watchdog threshold (minutes): running jobs whose last reported
  // step/progress is older than this are failed as stalled (through the
  // normal failure path, so flow retry policy applies). 0/absent = off.
  // Valid range on save: 0-10080.
  stall_timeout_minutes?: number;
  // Notification channels (additive to Discord; each empty = disabled).
  // Generic webhook: structured JSON event POSTs (job outcomes, operational
  // alerts, digest summaries) — payload shape in Docs/md/Operations.md.
  generic_webhook_url?: string;
  // ntfy: full publish endpoint (scheme+host+topic). The access token is
  // WRITE-ONLY: GET never returns it (has_ntfy_token flags its presence);
  // send it via SettingsSave.ntfy_token — empty keeps the stored value.
  ntfy_topic_url?: string;
  has_ntfy_token?: boolean;
  // SMTP email channel: enabled when host+from+to are all set; port 0
  // defaults to 587. The password is WRITE-ONLY like the ntfy token.
  smtp_host?: string;
  smtp_port?: number;
  smtp_user?: string;
  has_smtp_password?: boolean;
  smtp_from?: string;
  smtp_to?: string; // comma-separated recipients
  // Offsite backups (P4): S3 + SFTP destinations for DB snapshots. The
  // block's secrets are write-only (has_* flags on GET); see BackupOffsite.
  backup_offsite?: BackupOffsite;
  // Experimental download ingestion (P11): stage completed torrents'
  // source files into the scripts share. OFF by default; see Ingest.
  ingest?: Ingest;
  updated_at?: string;
}

// SettingsSave is the PUT /api/settings payload: the settings blob plus the
// write-only secrets (which never appear on GET). An empty/omitted secret
// keeps the stored value (the shares pattern). The has_* output flags are
// GET-only and must not be sent back. An OMITTED backup_offsite or ingest
// block keeps the stored block wholesale; a present block replaces
// non-secret fields.
export type SettingsSave = Omit<
  Settings,
  'has_ntfy_token' | 'has_smtp_password' | 'backup_offsite' | 'ingest'
> & {
  ntfy_token?: string;
  smtp_password?: string;
  backup_offsite?: BackupOffsiteSave;
  ingest?: IngestSave;
};

// ProvisionRun: one controller-driven Ansible provisioning attempt.
export interface ProvisionRun {
  id: number;
  host: string;
  port: number;
  scheme: string;
  winrm_user: string;
  node_name: string;
  status: 'queued' | 'running' | 'success' | 'failed';
  error?: string;
  log?: string;
  created_at: string;
  finished_at?: string;
}

// BackupInfo is one DB snapshot in <dataDir>/backups.
export interface BackupInfo {
  name: string;
  size_bytes: number;
  created_at: string;
  scheduled: boolean;
}

// BackupStatus is the scheduler state (GET /api/backup → status).
export interface BackupStatus {
  enabled: boolean;
  every_seconds: number;
  max_backups: number;
  last_run?: string;
  last_error?: string;
  next_run?: string;
}

// Offsite backup destinations (P4). GET shape: the destination secrets are
// WRITE-ONLY — has_* flags report presence, values never echo back. Send
// credentials via SettingsSave.backup_offsite (blank keeps the stored one).
export interface OffsiteS3 {
  enabled?: boolean;
  endpoint?: string; // host[:port] or http(s) URL; stored normalized
  bucket?: string;
  prefix?: string;
  access_key?: string;
  use_tls?: boolean;
}
export interface OffsiteSFTP {
  enabled?: boolean;
  host?: string;
  port?: number; // 0 = default 22
  user?: string;
  path?: string; // empty = the SSH home dir
  // Public host key pinned on every connection (REQUIRED when enabled).
  // Known-hosts style "ssh-ed25519 AAAA…" or bare base64; public material,
  // so it round-trips over the API (unlike the write-only secrets).
  host_key?: string;
}
export interface BackupOffsite {
  s3?: OffsiteS3;
  sftp?: OffsiteSFTP;
  // Per-destination retention: newest N snapshot generations kept
  // (snapshot + key companion together). 0 = default 30; range 1-365.
  keep_n?: number;
  // Bundle the shares key with each snapshot so offsite restore can
  // decrypt DB secrets. Default true. SECURITY: the key decrypts every
  // secret in the DB — only use trusted, access-controlled destinations.
  include_shares_key?: boolean;
  has_s3_secret_key?: boolean; // GET only
  has_sftp_password?: boolean; // GET only
  has_sftp_private_key?: boolean; // GET only
}
// Save shapes: write-only secrets in, has_* flags out (flags live at the
// block level on BackupOffsite, mirroring the top-level has_ntfy_token).
export interface OffsiteS3Save extends OffsiteS3 {
  secret_key?: string;
}
export interface OffsiteSFTPSave extends OffsiteSFTP {
  password?: string;
  private_key?: string; // unencrypted PEM
}
export interface BackupOffsiteSave {
  s3?: OffsiteS3Save;
  sftp?: OffsiteSFTPSave;
  keep_n?: number;
  include_shares_key?: boolean;
}

// Experimental download ingestion (P11). OFF by default. The controller
// polls a BitTorrent client for completed downloads and stages their
// source video files into the scripts share (<series>/<episode>/); filter
// scripts (.avs/.vpy) stay hand-authored and the scanner queues jobs once
// both are present. The password is WRITE-ONLY: GET returns has_password,
// never the value; send it via SettingsSave.ingest (blank keeps stored).
export interface Ingest {
  enabled?: boolean;
  provider?: 'qbittorrent' | 'rutorrent' | '';
  url?: string; // client web base URL
  username?: string; // optional (auth-bypassed clients need none)
  category?: string; // optional category/label filter
  // Category → series folder name overrides (JSON object). The episode
  // number is still parsed from the release name — never guessed.
  category_map?: Record<string, string>;
  poll_interval_seconds?: number; // 0 = default 60; range 10-3600
  // Controller-visible mount of the client's download directory (the
  // directory that directly contains the completed torrent folders).
  download_root?: string;
  has_password?: boolean; // GET only
}
// Save shape: the write-only password in, has_password out.
export interface IngestSave extends Omit<Ingest, 'has_password'> {
  password?: string;
}

// ShareKind is the transport backing a share mount.
export type ShareKind = 'nfs' | 'smb' | 's3';
// ShareRole is what the share provides to the pipeline.
export type ShareRole = 'scripts' | 'release';

// Share is a storage source (mounted for nfs/smb, per-job transfer for s3).
// The password is write-only: the API returns has_password instead of any
// credential material, so the edit form treats password as "replace only".
export interface Share {
  id: number;
  name: string;
  kind: ShareKind;
  role: ShareRole;
  server: string;
  path: string;
  port?: number;
  username?: string;
  region?: string;
  use_tls?: boolean;
  endpoint?: string;
  enabled: boolean;
  mount_path?: string;
  has_password?: boolean;
  created_at?: string;
  updated_at?: string;
}

// APIToken is a scoped token for external automation. The plaintext token
// is only returned by the create call (shown once); list responses carry
// metadata only.
export interface APIToken {
  id: number;
  name: string;
  scope: 'admin' | 'read';
  last_used_at?: string;
  created_at?: string;
}

// AuditEvent is one row of the audit log (GET /api/audit): who did what to
// which object. Detail is a small JSON snippet (already secret-scrubbed
// server-side). At is a server timestamp string.
export interface AuditEvent {
  id: number;
  at: string;
  actor: string;
  action: string;
  object: string;
  detail: string;
}

// JobETA is the response of GET /api/jobs/{id}/eta: a remaining-time
// prediction from the average duration of done jobs on the same flow.
// eta_sec < 0 means "no estimate" (insufficient history).
export interface JobETA {
  avg_sec: number;
  samples: number;
  elapsed_sec: number;
  eta_sec: number;
  progress?: number;
}

// UpdateManifest: the agent/lib/bin versions the controller wants deployed.
// Windows slots (agent_version/prev_agent_version) and Linux slots
// (linux_agent_version/prev_linux_agent_version) are independent: publishing
// to one platform never disturbs the other.
export interface UpdateManifest {
  agent_version: string;
  agent_sha256: string;
  // Previous agent release kept for one-click rollback (empty until a
  // second publish has rotated a payload into the rollback slot).
  prev_agent_version?: string;
  prev_agent_sha256?: string;
  // Linux agent slots — independent from the Windows slots above. Absent
  // when no Linux agent has been published yet.
  linux_agent_version?: string;
  linux_agent_sha256?: string;
  prev_linux_agent_version?: string;
  prev_linux_agent_sha256?: string;
  lib_version: number;
  lib_sha256: string;
  bin_version: number;
  bin_sha256: string;
  bin_size?: number;
}

// User is a management-plane account (login-based). PasswordHash is never
// serialized by the backend (json:"-"), so the API never exposes it.
export interface User {
  id: number;
  username: string;
  role: 'admin' | 'viewer';
  created_at?: string;
}

// UpdateCheck is the response of GET /api/updates/check: compares the running
// controller version to the latest GitHub release tag. A 502 means the
// upstream check failed (rate-limited, offline) — the UI shows a muted error.
export interface UpdateCheck {
  current: string;
  latest: string;
  update_available: boolean;
  release_url: string;
}

export interface StepTemplate {
  id: number;
  key: string;
  label: string;
  description: string;
  params: ParamDef[];
  powershell: string;
  builtin: boolean;
  created_at: string;
  updated_at: string;
}

export interface PairingCode {
  id: number;
  name_hint: string;
  expires_at: string;
  used_by?: number;
  created_at: string;
}

export interface FlowExport {
  flow: Flow;
  templates: StepTemplate[];
}

export interface Node {
  id: number;
  name: string;
  enabled: boolean;
  status: NodeStatus;
  // Routing label matched against series.node_group; '' = wildcard node
  // that accepts jobs from any series.
  group?: string;
  // Per-node job slots (1 = historical one-job rule). ActiveJobs is the
  // transient count of assigned/running jobs for the "1/2 slots" display.
  max_concurrent_jobs?: number;
  active_jobs?: number;
  agent_version: string;
  lib_version: number;
  bin_version?: number;
  tasks_since_boot: number;
  reboot_pending: boolean;
  last_seen: string | null;
  last_error?: string;
  online?: boolean;
  // Most recent metric snapshot the agent reported in its heartbeat (C3
  // phase). Absent for old agents and never-reported/offline nodes.
  last_metrics?: NodeMetrics;
  // Tail of the agent's own log as shipped in its latest heartbeat
  // (≤8 KiB, newest last). Absent/empty for old agents — gate the
  // "Agent log" UI on a non-empty value.
  agent_log?: string;
}

// NodeMetrics: the 8 live counters an encode agent reports each heartbeat.
// All fields are optional-ish in practice (old agents omit them entirely,
// hence last_metrics? on Node), but when present all eight arrive together.
// GPU fields are -1 when the host has no GPU; callers gate GPU UI on that.
export interface NodeMetrics {
  cpu_pct: number;
  mem_used_mb: number;
  mem_total_mb: number;
  disk_free_gb: number;
  gpu_util: number; // -1 = no GPU on host
  gpu_temp: number; // -1 = no GPU on host
  gpu_mem_used_mb: number; // -1 = no GPU on host
  encode_fps: number; // 0 = idle / not encoding
}

// NodeMetricSample is one row of GET /api/nodes/{id}/metrics?range=… — the
// same eight counters plus a ts. The backend emits ts in
// "2026-08-30 07:00:00" UTC shape (no Z); helpers.fmtTime appends Z when
// parsing, and the panel does the same.
export interface NodeMetricSample extends NodeMetrics {
  ts: string;
}

// StepTiming: one row of the agent's per-step completion report (B1/B2
// phases). Populated when the agent finishes and reports timing data;
// absent for old agents and early failures that never reported.
export interface StepTiming {
  step: string;
  started_at: string;
  duration_sec: number;
}

// JobLogStreamEvent: one frame of the SSE live-progress stream
// (GET /api/jobs/{id}/log/stream). "progress" frames carry the live
// step/percentage/log tail while a job runs; the terminal "final" frame
// carries status/error/exit_code and signals the stream is closing.
export interface JobLogStreamEvent {
  type: 'progress' | 'final';
  step?: string;
  progress?: number;
  log_tail?: string;
  status?: string;
  error?: string;
  exit_code?: number;
  full_log?: boolean;
}

export interface Job {
  id: number;
  series: string;
  episode: string;
  episode_dir: string;
  script_type: 'avs' | 'vpy';
  flow_id: number;
  status: JobStatus;
  node_id?: number;
  step?: string;
  progress?: number;
  exit_code: number;
  error?: string;
  log_tail?: string;
  outputs?: string[];
  step_timings?: StepTiming[];
  // ENCODE_METRIC key=value pairs from the job script (vmaf, bitrates,
  // sizes, durations). Absent/empty for jobs whose scripts report none.
  metrics?: Record<string, number>;
  // Phase D2: priority (0=Normal, 1=High) settable on PENDING jobs only.
  // retry_count + next_retry_at reflect the backend's auto-retry state.
  priority?: number;
  retry_count?: number;
  next_retry_at?: string | null;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
}

export interface Settings {
  group: string;
  tag: string;
  tasks_before_reboot: number;
  default_flow: string;
  scripts_root: string;
  release_root: string;
}

// Step metadata for the builder now comes from the controller's step-template
// registry (StepTemplate); the old hardcoded catalog was removed in phase 2.

// ---------- Fleet stats (Phase E) ----------
// GET /api/stats?range=24h|7d|30d|all — aggregate job-history snapshot
// computed in SQL from the existing jobs columns + joins. No new storage.

export interface StatsTotals {
  done: number;
  failed: number;
  cancelled: number;
  avg_duration_sec: number;
  // Media-seconds per encode-second over done jobs reporting duration_sec.
  // 0 = no qualifying jobs — render as "—", never "0.0×".
  avg_speedup: number;
}

export interface StatsNodeRow {
  node_id: number;
  name: string;
  done: number;
  failed: number;
  avg_duration_sec: number;
  avg_speedup: number;
}

export interface StatsFlowRow {
  flow_id: number;
  name: string;
  done: number;
  failed: number;
  avg_duration_sec: number;
  avg_speedup: number;
}

export interface StatsStepRow {
  step: string;
  count: number;
}

export interface StatsDayRow {
  date: string; // YYYY-MM-DD
  count: number;
}

// StatsRepeatRow: one stuck episode (failed job that burned ≥1 auto-retry).
// attempts = retry_count + 1 (initial run + retries). error is capped at
// 200 chars server-side.
export interface StatsRepeatRow {
  job_id: number;
  series: string;
  episode: string;
  node_id: number;
  node_name: string;
  step: string;
  attempts: number;
  error: string;
  finished_at: string;
}

export interface Stats {
  range_days: number; // 1|7|30|0(all)
  totals: StatsTotals;
  per_node: StatsNodeRow[];
  per_flow: StatsFlowRow[];
  failures_by_step: StatsStepRow[];
  per_day: StatsDayRow[];
  repeat_failures: StatsRepeatRow[];
}
