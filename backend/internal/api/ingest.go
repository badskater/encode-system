package api

import (
	"context"
	"fmt"

	"github.com/badskater/encode-system/backend/internal/ingest"
	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/store"
)

// ---------- Experimental download ingestion loop (P11) ----------

// StartIngestLoop launches the experimental download-ingestion poll loop
// bound to ctx (lifecycle mirrors StartStallWatchdogLoop). Each tick reads
// the LIVE settings, so enabling/configuring ingestion on the Settings
// page applies on the next tick without a restart; Enabled=false (the
// default) makes every tick a no-op.
//
// HA: on Postgres the loop body runs under store.RunLoopSupervisor with
// LockKeyIngest (1008) so exactly one replica polls the download client
// (two replicas would double-stage and double-notify; the ledger dedupes
// the staging but the work and alerts would still duplicate). On SQLite
// the supervisor is a transparent always-locked wrapper; ctx-cancel /
// SIGTERM stop is preserved.
func (s *Server) StartIngestLoop(ctx context.Context) {
	go store.RunLoopSupervisor(ctx, s.Store, s.Log, store.LockKeyIngest, "ingest", func(ctx context.Context) {
		ingest.RunLoop(ctx, s.Log, s.Store, s.ingestLiveConfig, s.ingestClientFactory(), s.onIngestStaged)
	})
}

// ingestLiveConfig reads the live settings for one poll cycle.
func (s *Server) ingestLiveConfig(ctx context.Context) ingest.Config {
	st := s.currentSettings(ctx)
	return ingest.Config{Settings: st.Ingest, ScriptsRoot: st.ScriptsRoot}
}

// ingestClientFactory returns the production client factory, or the test
// seam when one is injected (Server.IngestClient — same pattern as
// OffsiteDestinations). Construction is per-cycle so credential rotation
// applies without a restart; it is cheap (no network round-trip).
func (s *Server) ingestClientFactory() ingest.ClientFactory {
	if s.IngestClient != nil {
		return s.IngestClient
	}
	return func(in model.IngestSettings) (ingest.Client, error) {
		switch in.Provider {
		case model.IngestProviderQBit:
			return ingest.NewQBit(in.URL, in.Username, in.Password, s.Log)
		case model.IngestProviderRuTorrent:
			return ingest.NewRuTorrent(in.URL, in.Username, in.Password, s.Log)
		default:
			return nil, fmt.Errorf("unknown ingest provider %q", in.Provider)
		}
	}
}

// onIngestStaged audits one staged torrent (ingest.staged, actor
// "controller") and fans out an operational alert when any notification
// channel is configured. The alert tells the operator the NEXT manual
// step: author the .avs/.vpy filter script so the scanner can queue the
// encode — ingestion stages sources only, it never creates jobs.
//
// A torrent that staged ZERO files (Q9: another release already staged
// this episode) is audited as ingest.duplicate and gets NO success
// alert — the ledger row still dedupes it, but nothing new happened.
func (s *Server) onIngestStaged(ev ingest.StagedEvent) {
	ctx := context.Background()
	if len(ev.Files) == 0 {
		s.auditSystem(ctx, "ingest.duplicate", "ingest:"+ev.Provider+":"+ev.Hash, map[string]any{
			"name":        ev.Name,
			"series":      ev.Series,
			"episode":     ev.Episode,
			"staged_path": ev.StagedPath,
			"files":       0,
		})
		return
	}
	s.auditSystem(ctx, "ingest.staged", "ingest:"+ev.Provider+":"+ev.Hash, map[string]any{
		"name":        ev.Name,
		"series":      ev.Series,
		"episode":     ev.Episode,
		"staged_path": ev.StagedPath,
		"files":       len(ev.Files),
	})
	st := s.currentSettings(ctx)
	if !notify.AnyConfigured(st) {
		return // logs + audit only — nowhere to send an alert
	}
	content := fmt.Sprintf(
		"Ingest staged — %s Ep %s\nSource files are in the scripts share (%s). Author the .avs/.vpy filter script there to queue the encode.\nTorrent: %s",
		ev.Series, ev.Episode, ev.StagedPath, ev.Name)
	// Same seam as the node-alert/offsite paths: the injected Notifier
	// (tests) wins; production fans out to the live channels.
	if s.Notifier != nil {
		s.Notifier.Alert(ctx, content)
		return
	}
	notify.NewFanoutFromSettings(st, st.DiscordWebhook, s.mailSender(), s.Log).Alert(ctx, content)
}
