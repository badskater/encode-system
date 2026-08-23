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

// Step is one entry in a flow: a step template plus its parameters.
type Step struct {
	Type   StepType          `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

// Flow is a named, ordered list of steps rendered into a job script.
type Flow struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Steps     []Step    `json:"steps"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Node is one Windows encode machine.
type Node struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	TokenHash      string     `json:"-"`
	Enabled        bool       `json:"enabled"`
	Status         NodeStatus `json:"status"`
	AgentVersion   string     `json:"agent_version"`
	LibVersion     int64      `json:"lib_version"`
	TasksSinceBoot int        `json:"tasks_since_boot"`
	RebootPending  bool       `json:"reboot_pending"`
	// RebootIssuedAtTasks snapshots the counter when the reboot instruction
	// was issued. A heartbeat reporting fewer tasks proves the node rebooted.
	RebootIssuedAtTasks int    `json:"-"`
	LastSeen       *time.Time `json:"last_seen,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Job is one episode encode assignment.
type Job struct {
	ID         int64     `json:"id"`
	Series     string    `json:"series"`
	Episode    string    `json:"episode"` // e.g. "01"
	EpisodeDir string    `json:"episode_dir"` // path relative to scripts share, e.g. "Series Name/Ep 01"
	ScriptType string    `json:"script_type"` // "avs" or "vpy"
	ScriptFile string    `json:"script_file"` // detected filter script, e.g. "1080.vpy"
	FlowID     int64     `json:"flow_id"`
	Status     JobStatus `json:"status"`
	NodeID     int64     `json:"node_id,omitempty"`
	Step       string    `json:"step,omitempty"`
	Progress   float64   `json:"progress,omitempty"`
	ExitCode   int       `json:"exit_code"`
	Error      string    `json:"error,omitempty"`
	LogTail    string    `json:"log_tail,omitempty"`
	Outputs    []string   `json:"outputs,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Heartbeat is the periodic agent status report.
type Heartbeat struct {
	Node           string  `json:"node"`
	AgentVersion   string  `json:"agent_version"`
	LibVersion     int64   `json:"lib_version"`
	TasksSinceBoot int     `json:"tasks_since_boot"`
	JobID          int64   `json:"job_id,omitempty"`
	JobStatus      string  `json:"job_status,omitempty"`
	Step           string  `json:"step,omitempty"`
	StepProgress   float64 `json:"step_progress,omitempty"`
	LogTail        string  `json:"log_tail,omitempty"`
}

// JobPayload is what the controller hands to an agent to run a job.
type JobPayload struct {
	ID     int64             `json:"id"`
	Script string            `json:"script"` // rendered PowerShell
	Vars   map[string]string `json:"vars"`   // job variables for logging/context
	Flow   string            `json:"flow"`   // flow name, informational
}

// UpdateManifest describes the agent/lib versions the controller wants deployed.
type UpdateManifest struct {
	AgentVersion string `json:"agent_version"`
	AgentSHA256  string `json:"agent_sha256"`
	LibVersion   int64  `json:"lib_version"`
	LibSHA256    string `json:"lib_sha256"`
}

// HeartbeatReply is the controller's instruction channel to the agent.
type HeartbeatReply struct {
	Instruction string          `json:"instruction"` // none | job | reboot | update
	Job         *JobPayload     `json:"job,omitempty"`
	RebootDelay int             `json:"reboot_delay_seconds,omitempty"`
	Update      *UpdateManifest `json:"update,omitempty"`
}
