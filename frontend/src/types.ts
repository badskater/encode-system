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
  updated_at?: string;
}

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
export interface UpdateManifest {
  agent_version: string;
  agent_sha256: string;
  // Previous agent release kept for one-click rollback (empty until a
  // second publish has rotated a payload into the rollback slot).
  prev_agent_version?: string;
  prev_agent_sha256?: string;
  lib_version: number;
  lib_sha256: string;
  bin_version: number;
  bin_sha256: string;
  bin_size?: number;
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
}

export interface StatsNodeRow {
  node_id: number;
  name: string;
  done: number;
  failed: number;
  avg_duration_sec: number;
}

export interface StatsFlowRow {
  flow_id: number;
  name: string;
  done: number;
  failed: number;
  avg_duration_sec: number;
}

export interface StatsStepRow {
  step: string;
  count: number;
}

export interface StatsDayRow {
  date: string; // YYYY-MM-DD
  count: number;
}

export interface Stats {
  range_days: number; // 1|7|30|0(all)
  totals: StatsTotals;
  per_node: StatsNodeRow[];
  per_flow: StatsFlowRow[];
  failures_by_step: StatsStepRow[];
  per_day: StatsDayRow[];
}
