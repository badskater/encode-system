package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
)

// ctxBg returns a background context for startup seeding operations.
func ctxBg() context.Context { return context.Background() }

// handleHeartbeat processes an agent status report and decides the
// instruction to send back. Decision order matters:
//
//  1. Record node state (versions, task counter, current job progress).
//  2. Update is offered whenever the manifest outdates the node — but only
//     when the node is idle, so a running encode is never interrupted.
//  3. Reboot is issued once tasks_since_boot reaches the threshold and the
//     node has no active job.
//  4. Job assignment happens only for enabled, idle, below-threshold nodes.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, node *model.Node) {
	var hb model.Heartbeat
	if err := decodeJSON(r, &hb); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid heartbeat: "+err.Error())
		return
	}
	ctx := r.Context()

	// 1. Persist node state from the report. prevTasks is the counter from the
	// previous heartbeat — the agent counter only ever increases within a
	// boot, so a decrease proves the node came back from a reboot.
	prevTasks := node.TasksSinceBoot
	node.AgentVersion = hb.AgentVersion
	node.LibVersion = hb.LibVersion
	node.TasksSinceBoot = hb.TasksSinceBoot
	now := time.Now().UTC()
	node.LastSeen = &now
	node.LastError = ""

	hasActiveJob := false
	if hb.JobID > 0 {
		if job, err := s.Store.GetJob(ctx, hb.JobID); err == nil && job != nil && !job.Status.Terminal() {
			// Ownership check: a node may only report on its own job. Without
			// this, one node could overwrite or terminate another node's job.
			if job.NodeID != node.ID {
				s.Log.Warn("heartbeat reported foreign job", "node", node.Name,
					"job", job.ID, "owner", job.NodeID)
			} else {
				hasActiveJob = true
				if err := s.Store.UpdateJobStatus(ctx, job.ID, model.JobStatus(hb.JobStatus), hb.Step, hb.StepProgress, hb.LogTail); err != nil {
					s.Log.Warn("update job status", "err", err, "job", job.ID)
				}
			}
		}
	}

	// Post-reboot recovery: the counter DECREASED since the last heartbeat,
	// which only happens when the agent reset it while processing the reboot
	// instruction (it never decreases within a boot). Clear the flag so the
	// node rejoins the pool. While the flag is set and no decrease is seen,
	// the reboot instruction keeps being re-issued below — a missed packet
	// self-heals on the next heartbeat.
	if node.RebootPending && !hasActiveJob && node.TasksSinceBoot < prevTasks {
		node.RebootPending = false
		s.Log.Info("node returned after reboot", "node", node.Name,
			"tasks", node.TasksSinceBoot, "previous", prevTasks)
	}
	if hasActiveJob {
		node.Status = model.NodeBusy
	} else if node.RebootPending {
		node.Status = model.NodeReboot
	} else {
		node.Status = model.NodeIdle
	}
	if err := s.Store.UpdateNode(ctx, node); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist node state")
		return
	}

	// A disabled node gets no work, no reboot, no update — just an ack.
	if !node.Enabled {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}

	// 2. Reboot enforcement comes BEFORE updates: a node at its task limit is
	//    health-critical and must not be blocked behind an update the node
	//    hasn't applied yet. The instruction is re-issued on every idle
	//    heartbeat while the flag is set, so a missed packet self-heals.
	if !hasActiveJob && node.TasksSinceBoot >= s.Cfg.TasksBeforeReboot {
		if !node.RebootPending {
			node.RebootPending = true
			node.RebootIssuedAtTasks = node.TasksSinceBoot
			node.Status = model.NodeReboot
			if err := s.Store.UpdateNode(ctx, node); err != nil {
				writeErr(w, http.StatusInternalServerError, "persist reboot flag")
				return
			}
			s.Log.Info("node reached task limit, issuing reboot", "node", node.Name,
				"tasks", node.TasksSinceBoot, "limit", s.Cfg.TasksBeforeReboot)
		}
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "reboot", RebootDelay: 30})
		return
	}
	if !hasActiveJob && node.RebootPending {
		// Manual reboot or pending flag from a previous cycle: keep issuing.
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "reboot", RebootDelay: 30})
		return
	}

	// 3. Offer updates while idle.
	m := s.Update.Manifest()
	needsAgent := m.AgentVersion != "" && m.AgentVersion != node.AgentVersion
	needsLib := m.LibVersion > 0 && m.LibVersion != node.LibVersion
	if !hasActiveJob && (needsAgent || needsLib) {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "update", Update: &m})
		return
	}

	// 4. Assign a pending job if the node is idle and under the limit.
	if hasActiveJob || node.RebootPending || node.TasksSinceBoot >= s.Cfg.TasksBeforeReboot {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	jobs, err := s.Store.ListJobs(ctx, model.JobPending, 1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list pending jobs")
		return
	}
	if len(jobs) == 0 {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	job := jobs[0]
	payload, err := s.renderJob(ctx, job)
	if err != nil {
		s.Log.Error("render job failed", "job", job.ID, "err", err)
		s.Store.FinishJob(ctx, job.ID, model.JobFailed, -1, "render failed: "+err.Error(), nil, "")
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	if err := s.Store.AssignJob(ctx, job.ID, node.ID); err != nil {
		s.Log.Warn("assign job", "err", err, "job", job.ID, "node", node.Name)
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	s.Log.Info("assigned job", "job", job.ID, "node", node.Name, "episode", job.EpisodeDir)
	writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "job", Job: payload})
}

// renderJob builds the PowerShell payload for a job from its flow.
func (s *Server) renderJob(ctx context.Context, job *model.Job) (*model.JobPayload, error) {
	fl, err := s.Store.GetFlow(ctx, job.FlowID)
	if err != nil {
		return nil, err
	}
	vars := flow.Vars{
		BinDir:     s.Cfg.NodeBinDir,
		ScriptsDir: s.Cfg.NodeScriptsDir,
		ReleaseDir: s.Cfg.NodeReleaseDir,
		Group:      s.Cfg.Group,
		Tag:        s.Cfg.Tag,
	}
	script, err := flow.Render(fl, job, vars)
	if err != nil {
		return nil, err
	}
	return &model.JobPayload{
		ID:     job.ID,
		Script: script,
		Vars: map[string]string{
			"series": job.Series, "episode": job.Episode,
			"episode_dir": job.EpisodeDir, "script_type": job.ScriptType,
		},
		Flow: fl.Name,
	}, nil
}

// handleJobComplete records the agent's final job report.
func (s *Server) handleJobComplete(w http.ResponseWriter, r *http.Request, node *model.Node) {
	idStr := r.PathValue("id")
	jobID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	var rep struct {
		Status   string   `json:"status"` // done | failed
		ExitCode int      `json:"exit_code"`
		Error    string   `json:"error"`
		Outputs  []string `json:"outputs"`
		LogTail  string   `json:"log_tail"`
	}
	if err := decodeJSON(r, &rep); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid completion report")
		return
	}
	ctx := r.Context()
	job, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if job.NodeID != node.ID {
		writeErr(w, http.StatusForbidden, "job belongs to another node")
		return
	}
	// Idempotency: agents retry completions after timeouts; a job already in
	// a terminal state is answered with the recorded status, not re-finished.
	if job.Status.Terminal() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already_recorded"})
		return
	}
	status := model.JobDone
	if rep.Status != "done" {
		status = model.JobFailed
	}
	if err := s.Store.FinishJob(ctx, jobID, status, rep.ExitCode, rep.Error, rep.Outputs, rep.LogTail); err != nil {
		writeErr(w, http.StatusInternalServerError, "finish job")
		return
	}
	if err := s.Store.ReleaseNode(ctx, node.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "release node")
		return
	}
	s.Log.Info("job finished", "job", jobID, "status", status, "node", node.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// handleManifest returns the desired versions (agents compare and act).
func (s *Server) handleManifest(w http.ResponseWriter, _ *http.Request, _ *model.Node) {
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// handleDownloadAgent streams the stored agent binary.
func (s *Server) handleDownloadAgent(w http.ResponseWriter, r *http.Request, _ *model.Node) {
	f, err := s.Update.AgentPayload()
	if err != nil {
		writeErr(w, http.StatusNotFound, "no agent payload published")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	// Passing the request enables Range/If-Range so interrupted downloads
	// can resume instead of restarting from byte zero.
	http.ServeContent(w, r, "encode-agent.exe", time.Time{}, f)
}

// handleDownloadLib streams the stored EncodeLib.ps1.
func (s *Server) handleDownloadLib(w http.ResponseWriter, r *http.Request, _ *model.Node) {
	f, err := s.Update.LibPayload()
	if err != nil {
		writeErr(w, http.StatusNotFound, "no lib payload published")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, "EncodeLib.ps1", time.Time{}, f)
}

// ---------- UI handlers ----------

// handleListNodes returns all nodes with freshness computed.
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.Store.ListNodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list nodes")
		return
	}
	now := time.Now().UTC()
	type nodeView struct {
		*model.Node
		Online bool `json:"online"`
	}
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		online := n.LastSeen != nil && now.Sub(*n.LastSeen) < s.Cfg.StaleAfter
		if !online {
			n.Status = model.NodeOffline
		}
		out = append(out, nodeView{Node: n, Online: online})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateNode registers a node and returns its one-time token.
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate token")
		return
	}
	node, err := s.Store.CreateNode(r.Context(), req.Name, auth.HashToken(token))
	if err != nil {
		writeErr(w, http.StatusConflict, "node name taken")
		return
	}
	s.Log.Info("node registered", "node", req.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"node": node, "token": token})
}

// handlePatchNode toggles enabled/disabled (one task per system control).
func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad node id")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid patch")
		return
	}
	node, err := s.Store.GetNode(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	if req.Enabled != nil {
		node.Enabled = *req.Enabled
		if !node.Enabled {
			node.RebootPending = false // cancel pending reboot when paused
		}
	}
	if err := s.Store.UpdateNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "update node")
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleRebootNode flags a node for reboot on its next idle heartbeat.
func (s *Server) handleRebootNode(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad node id")
		return
	}
	node, err := s.Store.GetNode(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	node.RebootPending = true
	node.RebootIssuedAtTasks = node.TasksSinceBoot
	node.Status = model.NodeReboot
	if err := s.Store.UpdateNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "update node")
		return
	}
	s.Log.Info("manual reboot requested", "node", node.Name)
	writeJSON(w, http.StatusOK, node)
}

// handleListJobs lists jobs, optional ?status= filter.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	status := model.JobStatus(r.URL.Query().Get("status"))
	jobs, err := s.Store.ListJobs(r.Context(), status, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list jobs")
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleCreateJob manually creates a job for an episode dir with a flow.
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Series     string `json:"series"`
		EpisodeDir string `json:"episode_dir"`
		ScriptType string `json:"script_type"` // avs | vpy
		FlowID     int64  `json:"flow_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Series == "" || req.EpisodeDir == "" {
		writeErr(w, http.StatusBadRequest, "series and episode_dir required")
		return
	}
	if req.ScriptType != "avs" && req.ScriptType != "vpy" {
		writeErr(w, http.StatusBadRequest, "script_type must be avs or vpy")
		return
	}
	ctx := r.Context()
	fl, err := s.resolveFlow(ctx, req.FlowID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "flow: "+err.Error())
		return
	}
	job, err := s.Store.CreateJob(ctx, &model.Job{
		Series: req.Series, EpisodeDir: req.EpisodeDir,
		Episode:    flow.EpisodeNumber(req.EpisodeDir),
		ScriptType: req.ScriptType, FlowID: fl.ID,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create job")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// resolveFlow picks the flow by ID, falling back to the default flow name.
func (s *Server) resolveFlow(ctx context.Context, id int64) (*model.Flow, error) {
	if id > 0 {
		return s.Store.GetFlow(ctx, id)
	}
	return s.Store.FlowByName(ctx, s.Cfg.DefaultFlowName)
}

// handleGetJob returns one job with full detail.
func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	job, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleRetryJob re-queues a failed/cancelled/done job.
func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	n, err := s.Store.RetryJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "retry job")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusConflict, "job is not retryable (only failed/cancelled/done jobs can be retried)")
		return
	}
	job, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// handleCancelJob cancels a pending job.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	job, err := s.Store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	switch job.Status {
	case model.JobPending:
		// Pending jobs cancel directly.
	case model.JobAssigned:
		// Assigned but not yet running: cancel and free the node. A late
		// completion from the agent is absorbed by the idempotency guard.
		if err := s.Store.ReleaseNode(r.Context(), job.NodeID); err != nil {
			writeErr(w, http.StatusInternalServerError, "release node")
			return
		}
	default:
		writeErr(w, http.StatusConflict, "only pending/assigned jobs can be cancelled (running jobs must finish)")
		return
	}
	if _, err := s.Store.CancelJob(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "cancel job")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handleListFlows returns all flows.
func (s *Server) handleListFlows(w http.ResponseWriter, r *http.Request) {
	flows, err := s.Store.ListFlows(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list flows")
		return
	}
	writeJSON(w, http.StatusOK, flows)
}

// handleCreateFlow creates a flow from the UI builder.
func (s *Server) handleCreateFlow(w http.ResponseWriter, r *http.Request) {
	var req model.Flow
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid flow")
		return
	}
	if req.Name == "" || len(req.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, "name and at least one step required")
		return
	}
	// Server-controlled fields never come from the client.
	req.ID = 0
	req.CreatedAt = time.Time{}
	req.UpdatedAt = time.Time{}
	// Validate the flow renders before persisting it.
	dummy := &model.Job{ID: 0, Series: "Validate", Episode: "01", EpisodeDir: "Validate/Ep 01", ScriptType: "vpy"}
	if _, err := flow.Render(&req, dummy, flow.Vars{}); err != nil {
		writeErr(w, http.StatusBadRequest, "flow does not render: "+err.Error())
		return
	}
	fl, err := s.Store.CreateFlow(r.Context(), &req)
	if err != nil {
		writeErr(w, http.StatusConflict, "flow name taken")
		return
	}
	writeJSON(w, http.StatusCreated, fl)
}

// handleUpdateFlow replaces a flow definition.
func (s *Server) handleUpdateFlow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad flow id")
		return
	}
	var req model.Flow
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid flow")
		return
	}
	existing, err := s.Store.GetFlow(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "flow not found")
		return
	}
	if req.Name != "" {
		existing.Name = req.Name
	}
	if len(req.Steps) > 0 {
		existing.Steps = req.Steps
	}
	dummy := &model.Job{ID: 0, Series: "Validate", Episode: "01", EpisodeDir: "Validate/Ep 01", ScriptType: "vpy"}
	if _, err := flow.Render(existing, dummy, flow.Vars{}); err != nil {
		writeErr(w, http.StatusBadRequest, "flow does not render: "+err.Error())
		return
	}
	if err := s.Store.UpdateFlow(r.Context(), existing); err != nil {
		writeErr(w, http.StatusInternalServerError, "update flow")
		return
	}
	writeJSON(w, http.StatusOK, existing)
}

// handleDeleteFlow removes a flow. The configured default flow is protected:
// deleting it would break scanner job creation and new-job fallback.
func (s *Server) handleDeleteFlow(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad flow id")
		return
	}
	if existing, err := s.Store.GetFlow(r.Context(), id); err == nil && existing.Name == s.Cfg.DefaultFlowName {
		writeErr(w, http.StatusConflict, "the default flow cannot be deleted")
		return
	}
	if err := s.Store.DeleteFlow(r.Context(), id); err != nil {
		if strings.Contains(err.Error(), "referencing") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete flow")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSettings exposes non-secret runtime config to the UI.
func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"group":               s.Cfg.Group,
		"tag":                 s.Cfg.Tag,
		"tasks_before_reboot": s.Cfg.TasksBeforeReboot,
		"default_flow":        s.Cfg.DefaultFlowName,
		"scripts_root":        s.Cfg.ScriptsRoot,
		"release_root":        s.Cfg.ReleaseRoot,
	})
}
