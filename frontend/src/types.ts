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
  type: StepType;
  params?: Record<string, string>;
}

export interface Flow {
  id: number;
  name: string;
  steps: Step[];
  created_at: string;
  updated_at: string;
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
  last_seen: string;
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
  started_at?: string;
  finished_at?: string;
}

export interface Settings {
  group: string;
  tag: string;
  tasks_before_reboot: number;
  default_flow: string;
  scripts_root: string;
  release_root: string;
}

// Step metadata for the visual flow builder.
export interface StepMeta {
  type: StepType;
  label: string;
  description: string;
  params: { key: string; label: string; placeholder?: string }[];
}

export const STEP_CATALOG: StepMeta[] = [
  {
    type: 'source_rename',
    label: 'Rename source',
    description: 'Rename the raw upload to src.<ext>',
    params: [{ key: 'source_name', label: 'Source name', placeholder: 'src' }],
  },
  {
    type: 'dgindex',
    label: 'DGIndexNV index',
    description: 'Build src.dgi from the source',
    params: [],
  },
  {
    type: 'audio',
    label: 'Audio (eac3to → Opus)',
    description: 'Demux track to WAV, encode Opus',
    params: [
      { key: 'track', label: 'Track index', placeholder: '2' },
      { key: 'bitrate', label: 'Opus bitrate (kbps)', placeholder: '320' },
    ],
  },
  {
    type: 'encode',
    label: 'x265 encode',
    description: 'Encode the .avs/.vpy with the x265 fork',
    params: [{ key: 'x265_args', label: 'x265 arguments' }],
  },
  {
    type: 'mux',
    label: 'MKV mux',
    description: 'mkvmerge video + Opus audio',
    params: [],
  },
  {
    type: 'release_copy',
    label: 'Release copy',
    description: 'Copy MKV into [Group] Series - Raws [Tag]/',
    params: [],
  },
  {
    type: 'keyframes',
    label: 'Keyframes',
    description: 'ffmpeg → SCXvid keyframes file',
    params: [],
  },
];

export function stepMeta(type: StepType): StepMeta {
  return STEP_CATALOG.find((s) => s.type === type)!;
}
