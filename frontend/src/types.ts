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
}

export interface Series {
  id: number;
  name: string;
  flow_id: number; // 0 = default flow
  enabled: boolean;
  jobs?: number;
  created_at: string;
  updated_at: string;
}

export interface ParamDef {
  key: string;
  label: string;
  placeholder?: string;
  /** Runtime default applied by the step script when the flow omits the value. */
  default?: string;
  /** Widget type in the flow builder: 'text' (default), 'number', 'bool'. */
  type?: string;
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
  agent_version: string;
  lib_version: number;
  tasks_since_boot: number;
  reboot_pending: boolean;
  last_seen: string | null;
  last_error?: string;
  online?: boolean;
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
