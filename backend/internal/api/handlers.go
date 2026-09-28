package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/store"
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
	node.BinVersion = hb.BinVersion
	node.TasksSinceBoot = hb.TasksSinceBoot
	now := time.Now().UTC()
	node.LastSeen = &now
	node.LastError = ""

	// Persist the agent's latest resource sample into the node_metrics ring
	// table (24h retention, ≤500-point downsample on read). Old agents send
	// nil Metrics — skip entirely so the ring stays empty for them. A write
	// failure is logged and swallowed: metrics are observability data and
	// must never strand the agent without a heartbeat reply.
	node.Metrics = hb.Metrics
	if hb.Metrics != nil {
		if err := s.Store.InsertNodeMetric(ctx, node.ID, hb.Metrics); err != nil {
			s.Log.Warn("persist node metrics", "err", err, "node", node.Name)
		}
	}

	// An in-flight update sync (lib/bin/agent download+install) counts as
	// busy: assigning a job now could run it against a half-swapped toolchain.
	hasActiveJob := hb.Syncing
	// Per-node concurrency: process EVERY reported job, not just the
	// legacy single field. JobReports() normalizes old agents (legacy
	// fields) and new ones (Jobs array) into one list. reportedIDs feeds
	// the set-based orphan recovery below.
	reportedIDs := map[int64]bool{}
	for _, rep := range hb.JobReports() {
		job, err := s.Store.GetJob(ctx, rep.JobID)
		if err != nil || job == nil || job.Status.Terminal() {
			continue
		}
		// Ownership check: a node may only report on its own job. Without
		// this, one node could overwrite or terminate another node's job.
		if job.NodeID != node.ID {
			s.Log.Warn("heartbeat reported foreign job", "node", node.Name,
				"job", job.ID, "owner", job.NodeID)
			continue
		}
		reportedIDs[job.ID] = true
		hasActiveJob = true
		if err := s.Store.UpdateJobStatus(ctx, job.ID, model.JobStatus(rep.JobStatus), rep.Step, rep.StepProgress, rep.LogTail); err != nil {
			s.Log.Warn("update job status", "err", err, "job", job.ID)
		} else {
			// DB write landed: fan the live snapshot out to SSE
			// log-stream subscribers. Publish AFTER the store write
			// so stream events never run ahead of persisted state.
			s.publishProgress(job.ID, rep.Step, rep.StepProgress, rep.LogTail)
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
		node.RebootIssuedAt = nil
		s.Log.Info("node returned after reboot", "node", node.Name,
			"tasks", node.TasksSinceBoot, "previous", prevTasks)
	}

	// Orphan-job recovery: the heartbeat is authoritative about what a node is
	// running. A job becomes 'running' only once this node has acknowledged it
	// (reported it), so from then on every heartbeat must keep reporting it.
	// If the DB says this node owns a RUNNING job but the heartbeat reports no
	// active job, the node rebooted (or its agent restarted) mid-encode and the
	// job died silently — fail it so it can be retried instead of staying stuck
	// 'running' forever with the node phantom-busy. Assigned (not-yet-acknowledged)
	// jobs are untouched to avoid racing a fresh assignment.
	//
	// The orphan finish is routed through the same retry-decision helper as an
	// agent-reported failure (shouldAutoRetry): a flow with a retry policy
	// silently re-queues the orphan instead of alerting immediately, so a
	// controller-detected failure gets identical treatment to an agent-reported
	// one. When the orphan is auto-retried, notify is skipped (the helper already
	// scheduled the retry); otherwise it notifies as before.
	// Set-based orphan recovery for per-node concurrency: every DB-active
	// job this node owns that is NOT in the heartbeat's reported set and
	// has already been acknowledged (status running) died silently — fail
	// it so retry/steering can act. Assigned-but-unreported jobs are left
	// alone (fresh assignment race, same as the single-job rule).
	// activeCount below is derived from this same DB snapshot AFTER the
	// orphan finishes, so a node that just lost every job to orphaning is
	// correctly seen as empty.
	activeCount := 0
	if active, err := s.Store.ActiveJobsForNode(ctx, node.ID); err == nil {
		for _, orphan := range active {
			if orphan.Status != model.JobRunning || reportedIDs[orphan.ID] {
				continue
			}
			if err := s.Store.FinishJob(ctx, orphan.ID, model.JobFailed, -1,
				"node stopped reporting the job (reboot/restart) — orphaned; retry it", nil, ""); err != nil {
				s.Log.Warn("orphan job cleanup", "err", err, "job", orphan.ID)
				continue
			}
			// Re-read the job so shouldAutoRetry sees the just-stamped
			// failed state (RetryCount etc. from the DB row).
			failed, err := s.Store.GetJob(ctx, orphan.ID)
			if err != nil {
				// Transient DB error reading back the just-failed job: the
				// row is already 'failed' and visible in the UI, so the job
				// is not stranded — an operator can retry it manually.
				s.Log.Error("orphan recovery: GetJob after finish failed (job is already failed+visible in UI; skipped retry/notify this cycle)",
					"job", orphan.ID, "err", err)
			} else if s.shouldAutoRetry(ctx, failed, orphan.ID) {
				s.Log.Info("orphaned job auto-retried (node stopped reporting it)", "job", orphan.ID, "node", node.Name)
			} else {
				s.Log.Info("orphaned job failed (node stopped reporting it)", "job", orphan.ID, "node", node.Name)
				// Terminal SSE event for the orphan (mirrors the
				// completion path so open log streams close).
				s.publishFinal(orphan.ID, string(model.JobFailed), failed.Error, failed.ExitCode, failed.FullLog != "")
				s.notifyJobFinished(ctx, orphan.ID, "controller")
			}
		}
		// Survivors (still-running or freshly assigned) occupy slots.
		for _, j := range active {
			if !reportedIDs[j.ID] && j.Status == model.JobRunning {
				// Just failed as an orphan above — no longer active.
				continue
			}
			activeCount++
		}
	}

	// Grace-period expiry: a reboot attempt older than the grace window is
	// reset so the threshold logic below starts a fresh attempt (or frees the
	// node if its counter already reset). This guarantees no node can be
	// locked out forever by a stuck flag or a lost issue timestamp.
	if node.RebootPending && !hasActiveJob &&
		(node.RebootIssuedAt == nil || time.Since(*node.RebootIssuedAt) > s.Cfg.RebootGracePeriod) {
		node.RebootPending = false
		node.RebootIssuedAt = nil
		s.Log.Warn("reboot attempt expired, resetting", "node", node.Name,
			"tasks", node.TasksSinceBoot)
	}
	// Node status under per-node concurrency: busy only AT capacity (or
	// syncing). A node with free slots stays idle so the UI and the
	// stale-node heuristics treat it as available.
	maxConc := node.MaxConcurrentJobs
	if maxConc < 1 {
		maxConc = 1
	}
	atCapacity := hb.Syncing || activeCount >= maxConc
	if atCapacity {
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
	limit := s.Cfg.TasksBeforeReboot
	if st, err := s.Store.GetSettings(ctx); err == nil && st != nil && st.TasksBeforeReboot > 0 {
		limit = st.TasksBeforeReboot
	}
	if !hasActiveJob && node.TasksSinceBoot >= limit {
		if !node.RebootPending {
			node.RebootPending = true
			node.RebootIssuedAtTasks = node.TasksSinceBoot
			issued := now
			node.RebootIssuedAt = &issued
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

	// 3. Offer updates while idle. The agent compares each manifest field
	//    against its own state and syncs what differs: lib first (no restart
	//    needed), then the bin folder, then the agent binary (restart via the
	//    swap-bat trick). Re-issued every idle heartbeat until the node
	//    reports matching versions.
	m := s.Update.Manifest()
	needsAgent := m.AgentVersion != "" && m.AgentVersion != node.AgentVersion
	needsLib := m.LibVersion > 0 && m.LibVersion != node.LibVersion
	needsBin := m.BinVersion > 0 && m.BinVersion != node.BinVersion
	if !hasActiveJob && (needsAgent || needsLib || needsBin) {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "update", Update: &m})
		return
	}

	// 4. Assign pending jobs while the node has free slots (per-node
	//    concurrency). max=1 reproduces the historical one-job rule.
	freeSlots := maxConc - activeCount
	if hb.Syncing || freeSlots <= 0 || node.RebootPending || node.TasksSinceBoot >= s.Cfg.TasksBeforeReboot {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	// Disk-space soft drain: a node below settings.DiskAlertGB free space
	// gets no new assignments (the alert itself fires inside checkDisk with
	// a per-node cooldown). Running jobs continue — they may still fit, and
	// a genuinely full disk fails the encode which retries elsewhere.
	// Sits next to the drain check: both hold assignment, neither cancels.
	if s.checkDisk(ctx, node, &hb) {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	// Drain mode: a live settings flag (editable in the UI, no restart)
	// that pauses ALL job assignment fleet-wide. Running jobs keep running
	// to completion — nothing here cancels in-flight work — but no new job
	// is dispatched while the flag is on. Use it for host maintenance or
	// before pushing a new bin package so no node starts an encode against
	// a toolchain that's about to change. The check sits BEFORE the
	// NextAssignableJob lookup so a draining fleet doesn't even dequeue.
	// Toggled off in the UI, the next idle heartbeat resumes assignment.
	if s.currentSettings(ctx).DrainMode {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	// NextAssignableJobForNode gates on next_retry_at (a backoff-pending
	// job is not ready yet), orders by priority DESC then id ASC so urgent
	// jobs dispatch first and, within a priority tier, the oldest job wins
	// (true FIFO), steers away from jobs this node most recently failed,
	// and honors group routing + series pause. The loop fills every free
	// slot in one heartbeat reply; AssignJob re-checks capacity inside its
	// transaction, so concurrent heartbeats can never overshoot the cap.
	var payloads []*model.JobPayload
	for i := 0; i < freeSlots; i++ {
		job, err := s.Store.NextAssignableJobForNode(ctx, node.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list pending jobs")
			return
		}
		if job == nil {
			break // queue drained
		}
		payload, err := s.renderJob(ctx, job)
		if err != nil {
			s.Log.Error("render job failed", "job", job.ID, "err", err)
			s.Store.FinishJob(ctx, job.ID, model.JobFailed, -1, "render failed: "+err.Error(), nil, "")
			s.notifyJobFinished(ctx, job.ID, "controller")
			continue // try the next candidate with the remaining slots
		}
		if err := s.Store.AssignJob(ctx, job.ID, node.ID); err != nil {
			// Lost a race (another heartbeat took the slot or the node
			// filled up) — stop assigning; the next heartbeat re-evaluates.
			s.Log.Warn("assign job", "err", err, "job", job.ID, "node", node.Name)
			break
		}
		s.Log.Info("assigned job", "job", job.ID, "node", node.Name, "episode", job.EpisodeDir)
		payloads = append(payloads, payload)
	}
	if len(payloads) == 0 {
		writeJSON(w, http.StatusOK, model.HeartbeatReply{Instruction: "none"})
		return
	}
	reply := model.HeartbeatReply{Instruction: "job", Job: payloads[0]}
	if len(payloads) > 1 {
		// Multi-slot agents read Jobs; Job is still set (payloads[0]) so
		// an old agent that somehow has free slots (impossible at max=1)
		// would still run exactly one job.
		reply.Instruction = "jobs"
		reply.Jobs = payloads
	}
	writeJSON(w, http.StatusOK, reply)
}

// renderJob builds the PowerShell payload for a job from its flow.
func (s *Server) renderJob(ctx context.Context, job *model.Job) (*model.JobPayload, error) {
	fl, err := s.Store.GetFlow(ctx, job.FlowID)
	if err != nil {
		return nil, err
	}
	// Remote path mapping comes from the LIVE settings (editable in the UI),
	// falling back to the env-provided defaults when nothing was saved yet.
	st := s.currentSettings(ctx)
	tag := st.Tag
	// Per-series tag override: a series with its own quality tag (e.g. a 4K
	// re-encode run of a previously-1080p show) names its outputs and release
	// folder after that tag instead of the global one.
	if sr, err := s.Store.SeriesByName(ctx, job.Series); err == nil && sr.Tag != "" {
		tag = sr.Tag
	}
	vars := flow.Vars{
		BinDir:         st.NodeBinDir,
		ScriptsDir:     st.NodeScriptsDir,
		ReleaseDir:     st.NodeReleaseDir,
		Group:          st.Group,
		Tag:            tag,
		DiscordWebhook: s.discordWebhook(ctx),
	}
	// S3-backed roles: the agent stages per job (download sources →
	// encode → upload outputs) instead of reading mounts. Scripts/Release
	// vars point INTO the per-job staging dir via the {{JOBDIR}}
	// placeholder (the controller never knows node-local paths; the agent
	// expands it against its jobDir). Mount-backed roles keep settings
	// dirs untouched, so mixed farms work per-role.
	var s3spec *model.S3Transfer
	scriptsShare, _ := s.Store.ShareForRole(ctx, model.ShareRoleScripts)
	releaseShare, _ := s.Store.ShareForRole(ctx, model.ShareRoleRelease)
	if scriptsShare != nil && scriptsShare.Kind == model.ShareS3 {
		vars.ScriptsDir = "{{JOBDIR}}/scripts"
		s3spec = newS3SpecFromShare(s3spec, scriptsShare)
		s3spec.Downloads = append(s3spec.Downloads, model.S3Download{
			Bucket: scriptsShare.Path, Prefix: job.EpisodeDir,
			LocalDir: "{{JOBDIR}}/scripts",
		})
	}
	if releaseShare != nil && releaseShare.Kind == model.ShareS3 {
		vars.ReleaseDir = "{{JOBDIR}}/release"
		// A release share without a scripts share still needs a spec.
		s3spec = newS3SpecFromShare(s3spec, releaseShare)
		s3spec.Uploads = append(s3spec.Uploads, model.S3Upload{
			Bucket: releaseShare.Path, Prefix: job.EpisodeDir,
			LocalDir: "{{JOBDIR}}/release",
		})
	}
	script, err := flow.Render(fl, job, vars, s.storeResolver())
	if err != nil {
		return nil, err
	}
	episode := job.Episode
	if episode == "" {
		episode = flow.EpisodeNumber(job.EpisodeDir)
	}
	return &model.JobPayload{
		ID:     job.ID,
		Script: script,
		Vars: map[string]string{
			"series": job.Series, "episode": episode,
			"episode_dir": job.EpisodeDir, "script_type": job.ScriptType,
			// Expected mux artifact so the agent can verify/report the real
			// output file instead of the episode directory itself.
			"expected_output": flow.OutputName(job.Series, episode, vars.Tag),
		},
		Flow: fl.Name,
		S3:   s3spec,
	}, nil
}

// newS3SpecFromShare returns spec (creating it from the share's connection
// fields when nil) — both roles usually point at one MinIO/RGW instance,
// so the first s3 share seen supplies endpoint/creds.
func newS3SpecFromShare(spec *model.S3Transfer, sh *model.Share) *model.S3Transfer {
	if spec != nil {
		return spec
	}
	return &model.S3Transfer{
		Endpoint:  s3Endpoint(sh),
		Region:    sh.Region,
		AccessKey: sh.Username,
		SecretKey: sh.Password,
		UseTLS:    sh.UseTLS,
	}
}

// s3Endpoint normalizes a share's endpoint to host[:port] (minio-go wants
// no scheme): a full URL in Endpoint wins, else Server[:Port].
func s3Endpoint(sh *model.Share) string {
	ep := strings.TrimSpace(sh.Endpoint)
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	ep = strings.TrimSuffix(ep, "/")
	if ep != "" {
		return ep
	}
	if sh.Port > 0 {
		return fmt.Sprintf("%s:%d", sh.Server, sh.Port)
	}
	return sh.Server
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
		Status      string             `json:"status"` // done | failed
		ExitCode    int                `json:"exit_code"`
		Error       string             `json:"error"`
		Outputs     []string           `json:"outputs"`
		LogTail     string             `json:"log_tail"`
		LogFull     string             `json:"log_full"`     // v2: full captured run.log (agent omits on old builds → "")
		StepTimings []model.StepTiming `json:"step_timings"` // v2: per-step start/duration (old agents → nil)
		Metrics     map[string]float64 `json:"metrics"`      // v3: ENCODE_METRIC pairs (old agents → nil)
	}
	if err := decodeJSONLimit(r, &rep, maxCompleteBodyBytes); err != nil {
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
	if err := s.Store.FinishJobWithReport(ctx, jobID, status, rep.ExitCode, rep.Error, rep.Outputs, rep.LogTail, rep.LogFull, rep.StepTimings, rep.Metrics); err != nil {
		// FinishJobWithReport is guarded by status IN ('assigned','running').
		// A zero-rows match means the job left the live state between the
		// Terminal() pre-check above and this write (concurrent cancel, or a
		// duplicate completion that raced past the check). Answer
		// idempotently — the job is already terminal, so this report is a
		// no-op: do NOT notify or retry (both have already happened or will
		// never happen). This is the same shape as the Terminal() fast path.
		if errors.Is(err, store.ErrJobNotFinishable) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "already_recorded"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "finish job")
		return
	}
	if err := s.Store.ReleaseNode(ctx, node.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "release node")
		return
	}
	s.Log.Info("job finished", "job", jobID, "status", status, "node", node.Name)

	// Per-flow automatic retry: a failed job whose flow has a retry budget
	// (MaxRetries > 0) and remaining attempts (RetryCount < MaxRetries) is
	// silently re-queued as pending with a backoff gate instead of alerting.
	// The node is already freed above (ReleaseNode), so the retry does not
	// hold the box. When retries are exhausted, the flow has no policy, or
	// the flow vanished, the job notifies as before — the notify path reads
	// RetryCount off the job so the alert says "(after N retries)".
	//
	// Crash window: if the controller crashes between FinishJobWithReport
	// (above) and shouldAutoRetry/ScheduleJobRetry (below), the job stays
	// in the 'failed' state with no backoff gate — it will NOT be retried
	// until an operator retries it manually via the UI. This is by design:
	// the alternative (retrying on the next boot) would require a startup
	// scan, and a job that failed legitimately would be re-dispatched into
	// the same failure. The manual retry path (RetryJob) recovers it.
	if status == model.JobFailed && s.shouldAutoRetry(ctx, job, jobID) {
		// Auto-retried: NOT terminal — the stream stays open and will keep
		// relaying progress when the retry is re-assigned. No final event.
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}
	// Terminal SSE event: closes any open log streams for this job. Only
	// fires on the no-retry outcome (the auto-retry branch above returned).
	s.publishFinal(jobID, string(status), rep.Error, rep.ExitCode, rep.LogFull != "")
	s.notifyJobFinished(ctx, jobID, node.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// shouldAutoRetry decides whether a just-failed job is silently re-queued
// per its flow's retry policy, and performs the re-queue when so. Returns
// true when the job was re-queued (caller must skip notification); false
// when the job should notify (retries exhausted, no policy, or the flow
// could not be resolved). A GetFlow error or a vanished flow is treated as
// no-retry-policy so a broken flow lookup never strands a job.
//
// This is the shared retry-decision helper: both handleJobComplete
// (agent-reported failure) and the orphan-recovery path in handleHeartbeat
// (controller-detected failure) call it so the same logical failure gets
// identical treatment — auto-retry when a policy exists, notify otherwise.
// Without this, an agent-reported failure silently retried while a
// controller-detected orphan alerted immediately.
func (s *Server) shouldAutoRetry(ctx context.Context, job *model.Job, jobID int64) bool {
	fl, err := s.Store.GetFlow(ctx, job.FlowID)
	if err != nil || fl == nil || fl.MaxRetries <= 0 {
		return false // no retry policy → notify
	}
	if job.RetryCount >= fl.MaxRetries {
		return false // budget exhausted → notify (carries retry count)
	}
	// Backoff: the flow's configured minutes, clamped to a 24h ceiling.
	// The clamp must happen on the MINUTES value BEFORE the multiply: a
	// huge RetryBackoffMinutes (e.g. 999,999,999,999) multiplied by
	// time.Minute overflows time.Duration to a negative Duration, which
	// stamps next_retry_at in the PAST and causes instant re-dispatch.
	// 24h (1440 minutes) is generous for any legitimate backoff; an
	// operator who wants to park a job longer can disable it and retry
	// manually.
	minutes := fl.RetryBackoffMinutes
	if minutes <= 0 {
		minutes = 1
	}
	if minutes > 1440 {
		minutes = 1440
	}
	backoff := time.Duration(minutes) * time.Minute
	next := time.Now().UTC().Add(backoff)
	nextRetryCount := job.RetryCount + 1
	if err := s.Store.ScheduleJobRetry(ctx, jobID, nextRetryCount, next); err != nil {
		s.Log.Warn("schedule job retry", "err", err, "job", jobID)
		return false // scheduling failed → fall through to notify
	}
	s.Log.Warn("job auto-retry scheduled", "job", jobID, "retry", nextRetryCount,
		"max", fl.MaxRetries, "backoff", backoff, "next_retry_at", next)
	return true
}

// notifyJobFinished fires the outcome alert for a job that just reached a
// terminal state. The job is re-read so the alert carries the recorded error
// and timestamps. The notifier is resolved from the LIVE settings on every
// call (Settings-page webhook edits take effect immediately, no restart);
// an empty webhook is a no-op. When s.Notifier is set (test injection), it
// overrides the live resolution so tests can assert whether a notification
// fired.
//
// Per-series mute: a job whose series has Notify=false is skipped entirely —
// no direct alert AND no digest buffering. The mute check runs BEFORE the
// digest branch so a muted series never enters the buffer. An unknown series
// (scanner created the job before the series row existed, or the row was
// deleted) is treated as notify=true so muting is always an opt-in, never a
// silent default.
//
// Digest mode: when settings.NotifyDigest is ON (and the series is not
// muted), the outcome is pushed to the in-memory digest buffer and no direct
// alert fires — StartDigestLoop's hourly ticker flushes a single summary.
// When OFF, behavior is unchanged (immediate per-job alert). The buffer is
// initialized in New, so s.digest and s.digest.buf are never nil here.
func (s *Server) notifyJobFinished(ctx context.Context, jobID int64, nodeName string) {
	j, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		return
	}
	// Per-series mute: resolve the series row and skip when Notify is false.
	// This runs BEFORE the digest check so a muted series is never buffered.
	// A lookup miss (unknown series) falls through to notify — muting is
	// opt-in per series, never a default. The row also carries the optional
	// per-series webhook override used on the direct-alert path below.
	var seriesHook string
	if sr, err := s.Store.SeriesByName(ctx, j.Series); err == nil && sr != nil {
		if !sr.Notify {
			s.Log.Debug("series muted — skipping notify", "job", jobID, "series", j.Series)
			return
		}
		seriesHook = sr.WebhookURL
	}
	st := s.currentSettings(ctx)
	// Digest mode: buffer the outcome for an hourly summary instead of
	// alerting per job. The buffer is built in New, so s.digest.buf is
	// never nil; the guard is defensive against a misconfigured server.
	if st.NotifyDigest {
		if s.digest != nil && s.digest.buf != nil {
			s.digest.buf.Push(notify.DigestEntry{
				JobID:    j.ID,
				Series:   j.Series,
				Episode:  j.Episode,
				Status:   j.Status,
				NodeName: nodeName,
				At:       time.Now().UTC(),
			})
		}
		return // no direct post; the digest loop flushes a summary
	}
	if s.Notifier != nil {
		s.Notifier.JobFinished(ctx, j, nodeName)
		return
	}
	// Per-series webhook override wins over the global one (routed alerts
	// go to that series' channel). The injected s.Notifier above still
	// takes precedence so tests keep a single recording seam.
	hook := st.DiscordWebhook
	if seriesHook != "" {
		hook = seriesHook
	}
	notify.NewDiscordWithLink(hook, st.ControllerURL, s.Log).JobFinished(ctx, j, nodeName)
}

// discordWebhook returns the effective Discord webhook. The persisted
// settings row is authoritative once it exists — including a blank value,
// which lets the operator turn notifications OFF without editing env. With
// no saved row, the env default the controller booted with applies.
// currentSettings implements exactly that merge.
func (s *Server) discordWebhook(ctx context.Context) string {
	return s.currentSettings(ctx).DiscordWebhook
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
		Online      bool               `json:"online"`
		LastMetrics *model.NodeMetrics `json:"last_metrics,omitempty"`
	}
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		online := n.LastSeen != nil && now.Sub(*n.LastSeen) < s.Cfg.StaleAfter
		if !online {
			n.Status = model.NodeOffline
		}
		// Surface the last-reported metrics sample so the node list view
		// can render a current snapshot without a separate fetch. nil when
		// the node has never sent metrics (old agent or fresh registration).
		//
		// This is an N+1: one LatestNodeMetric query per node. That is
		// acceptable at fleet scale (~2-20 nodes): each query is a single
		// index seek on idx_metrics_node_ts, and a single-query
		// join/window optimization is deferred (YAGNI until the fleet
		// outgrows dozens of nodes).
		lm, err := s.Store.LatestNodeMetric(r.Context(), n.ID)
		if err != nil {
			s.Log.Warn("latest node metric", "err", err, "node", n.Name)
		}
		out = append(out, nodeView{Node: n, Online: online, LastMetrics: lm})
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
	s.audit(r, "node.create", auditObject("node", node.ID), map[string]any{"name": req.Name})
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
		Enabled           *bool   `json:"enabled"`
		Group             *string `json:"group"`
		MaxConcurrentJobs *int    `json:"max_concurrent_jobs"`
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
	// Group is field-scoped (SetNodeGroup) so a concurrent heartbeat's
	// UpdateNode (status/last_seen) can't clobber it and vice versa.
	if req.Group != nil {
		if err := s.Store.SetNodeGroup(r.Context(), id, *req.Group); err != nil {
			writeErr(w, http.StatusInternalServerError, "update node group")
			return
		}
		node.Group = *req.Group
	}
	// Concurrency cap: validated here (1-8) before the field-scoped write;
	// the store clamps too (defense in depth for direct callers).
	if req.MaxConcurrentJobs != nil {
		if *req.MaxConcurrentJobs < 1 || *req.MaxConcurrentJobs > 8 {
			writeErr(w, http.StatusBadRequest, "max_concurrent_jobs must be 1-8")
			return
		}
		if err := s.Store.SetNodeMaxConcurrent(r.Context(), id, *req.MaxConcurrentJobs); err != nil {
			writeErr(w, http.StatusInternalServerError, "update node concurrency")
			return
		}
		node.MaxConcurrentJobs = *req.MaxConcurrentJobs
	}
	if req.Enabled != nil || req.Group != nil || req.MaxConcurrentJobs != nil {
		s.audit(r, "node.update", auditObject("node", id), map[string]any{
			"enabled": req.Enabled, "group": req.Group, "max_concurrent_jobs": req.MaxConcurrentJobs,
		})
	}
	if err := s.Store.UpdateNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "update node")
		return
	}
	writeJSON(w, http.StatusOK, node)
}

// handleDeleteNode removes a node registration (used before re-provisioning
// a host whose name is already registered — pairing fails on name collision).
// Busy nodes cannot be deleted: their job would be orphaned mid-encode.
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
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
	// Atomic busy-guard: between the GetNode above and this delete, a job
	// could dispatch and flip the node to busy — the conditional DELETE
	// closes that window instead of orphaning an in-flight encode.
	n, err := s.Store.DeleteNodeIfNotBusy(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete node")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusConflict, "cannot delete a busy node")
		return
	}
	s.Log.Info("node deleted", "node", node.Name, "id", id)
	s.audit(r, "node.delete", auditObject("node", id), map[string]any{"name": node.Name})
	w.WriteHeader(http.StatusNoContent)
}

// handleNodeMetrics returns the persisted metric samples for a node over a
// time range (?range=1h|6h|24h, default 1h). Results are downsampled to
// ≤500 points server-side so the dashboard payload is bounded regardless of
// the raw heartbeat cadence. The node must exist (404 otherwise).
func (s *Server) handleNodeMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad node id")
		return
	}
	if _, err := s.Store.GetNode(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "node not found")
		return
	}
	// Parse the range param. Default 1h; accepted values 1h/6h/24h. An
	// unknown value falls back to 1h rather than 400-ing — the dashboard
	// links are generated by the UI, so a bad value is a stale bookmark,
	// not a user typo worth hard-stopping on.
	dur := 1 * time.Hour
	switch r.URL.Query().Get("range") {
	case "6h":
		dur = 6 * time.Hour
	case "24h":
		dur = 24 * time.Hour
	}
	samples, err := s.Store.ListNodeMetrics(r.Context(), id, time.Now().UTC().Add(-dur), 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list node metrics")
		return
	}
	writeJSON(w, http.StatusOK, samples)
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
	issued := time.Now().UTC()
	node.RebootIssuedAt = &issued
	node.Status = model.NodeReboot
	if err := s.Store.UpdateNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "update node")
		return
	}
	s.Log.Info("manual reboot requested", "node", node.Name)
	s.audit(r, "node.reboot", auditObject("node", id), map[string]any{"name": node.Name})
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
		ScriptFile string `json:"script_file"` // optional, e.g. "2160.avs"; blank = "<type default>.<type>"
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
	fl, err := s.resolveFlow(ctx, req.FlowID, req.Series)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "flow: "+err.Error())
		return
	}
	job, err := s.Store.CreateJob(ctx, &model.Job{
		Series: req.Series, EpisodeDir: req.EpisodeDir,
		Episode:    flow.EpisodeNumber(req.EpisodeDir),
		ScriptType: req.ScriptType, ScriptFile: req.ScriptFile, FlowID: fl.ID,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create job")
		return
	}
	writeJSON(w, http.StatusCreated, job)
}

// resolveFlow picks the flow by ID; with no explicit ID it honors the
// series' flow selection, then the flagged default flow, then the configured
// default name.
func (s *Server) resolveFlow(ctx context.Context, id int64, seriesName string) (*model.Flow, error) {
	if id > 0 {
		return s.Store.GetFlow(ctx, id)
	}
	if seriesName != "" {
		if sr, err := s.Store.SeriesByName(ctx, seriesName); err == nil && sr.FlowID > 0 {
			if fl, err := s.Store.GetFlow(ctx, sr.FlowID); err == nil {
				return fl, nil
			}
		}
	}
	if fl, err := s.Store.DefaultFlow(ctx); err == nil {
		return fl, nil
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

// handleGetJobLog serves the full captured run.log for a job as raw text.
// A job with no recorded log (old agent, or a finish path that carries no
// log) answers 404 with an explicit message so the UI can distinguish
// "no log" from "log empty". Admin-only: the log can hold paths and errors.
func (s *Server) handleGetJobLog(w http.ResponseWriter, r *http.Request) {
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
	if job.FullLog == "" {
		writeErr(w, http.StatusNotFound, "no log recorded for this job")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(job.FullLog))
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
	s.audit(r, "job.retry", auditObject("job", id), map[string]any{"episode_dir": job.EpisodeDir})
	writeJSON(w, http.StatusOK, job)
}

// handlePatchJob changes a pending job's flow and/or priority before it
// starts. Assigned or running jobs refuse the change — their script is
// already on (or bound for) a node, so swapping flows mid-flight would be
// meaningless. Priority is the D2 queue weight (higher = dispatched sooner);
// landing the API now is cheap since the handler is already open and the
// assignment path already orders by priority.
func (s *Server) handlePatchJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	var req struct {
		FlowID   *int64 `json:"flow_id"`
		Priority *int   `json:"priority"`
	}
	if err := decodeJSON(r, &req); err != nil || (req.FlowID == nil && req.Priority == nil) {
		writeErr(w, http.StatusBadRequest, "flow_id or priority required")
		return
	}
	ctx := r.Context()
	job, err := s.Store.GetJob(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if job.Status != model.JobPending {
		writeErr(w, http.StatusConflict, "only pending jobs can be patched")
		return
	}
	if req.FlowID != nil {
		if _, err := s.Store.GetFlow(ctx, *req.FlowID); err != nil {
			writeErr(w, http.StatusBadRequest, "flow not found")
			return
		}
	}
	// Atomic single-UPDATE patch: both flow_id and priority land in one guarded
	// statement (WHERE id=? AND status='pending'), closing the race window
	// the old two-call SetJobFlow+SetJobPriority sequence had (between the
	// two writes the job could be assigned, leaving the second write a
	// silent no-op or partially applied). A zero-rows match means the job
	// left the pending state between the status check above and this write
	// (concurrent assignment) — answer 409, the honest result.
	changed, err := s.Store.PatchPendingJob(ctx, id, req.FlowID, req.Priority)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "patch pending job")
		return
	}
	if !changed {
		writeErr(w, http.StatusConflict, "job is no longer pending")
		return
	}
	job, err = s.Store.GetJob(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	s.audit(r, "job.patch", auditObject("job", id), map[string]any{"flow_id": req.FlowID, "priority": req.Priority})
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
		// Order matters under per-node concurrency: ReleaseNode recomputes
		// idle/busy from the remaining active jobs, so the job must leave
		// 'assigned' state BEFORE the node is released.
	default:
		writeErr(w, http.StatusConflict, "only pending/assigned jobs can be cancelled (running jobs must finish)")
		return
	}
	n, err := s.Store.CancelJob(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cancel job")
		return
	}
	if job.Status == model.JobAssigned && n > 0 {
		if err := s.Store.ReleaseNode(r.Context(), job.NodeID); err != nil {
			writeErr(w, http.StatusInternalServerError, "release node")
			return
		}
	}
	s.audit(r, "job.cancel", auditObject("job", id), map[string]any{"episode_dir": job.EpisodeDir})
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
	// Validate every step resolves before persisting the flow.
	if err := flow.ValidateForRender(&req, s.storeResolver()); err != nil {
		writeErr(w, http.StatusBadRequest, "flow invalid: "+err.Error())
		return
	}
	fl, err := s.Store.CreateFlow(r.Context(), &req)
	if err != nil {
		writeErr(w, http.StatusConflict, "flow name taken")
		return
	}
	s.audit(r, "flow.create", auditObject("flow", fl.ID), map[string]any{"name": fl.Name, "steps": len(fl.Steps)})
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
	// PUT semantics replace the policy: copy the retry fields unconditionally
	// so a PUT with max_retries=0 (policy OFF) persists, matching the frontend
	// which always sends both fields. Without this, a PUT that turns the
	// retry policy OFF would leave the stale old values in options_json.
	existing.MaxRetries = req.MaxRetries
	existing.RetryBackoffMinutes = req.RetryBackoffMinutes
	if err := flow.ValidateForRender(existing, s.storeResolver()); err != nil {
		writeErr(w, http.StatusBadRequest, "flow invalid: "+err.Error())
		return
	}
	if err := s.Store.UpdateFlow(r.Context(), existing); err != nil {
		writeErr(w, http.StatusInternalServerError, "update flow")
		return
	}
	s.audit(r, "flow.update", auditObject("flow", id), map[string]any{"name": existing.Name, "steps": len(existing.Steps)})
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
	s.audit(r, "flow.delete", auditObject("flow", id), nil)
	w.WriteHeader(http.StatusNoContent)
}

// Settings handlers live in handlers_settings.go.
