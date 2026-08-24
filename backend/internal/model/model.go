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
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Steps     []Step    `json:"steps"`
	IsDefault bool      `json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
	RebootIssuedAtTasks int `json:"-"`
	// RebootIssuedAt records when the instruction was issued; after a grace
	// period the flag expires so a node cannot be locked out forever.
	RebootIssuedAt *time.Time `json:"-"`
	LastSeen       *time.Time `json:"last_seen,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
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
}

// Series is a registered show/folder on the scripts share with its own flow
// assignment and enable state. Episodes of a series may run on any enabled
// node (the queue distributes one job per idle node automatically).
type Series struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`    // matches the share folder name exactly
	FlowID    int64     `json:"flow_id"` // 0 = use the default flow
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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
