package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/notify"
	"github.com/badskater/encode-system/backend/internal/offsite"
)

// offsiteTimeout bounds one offsite run (uploads + retention across all
// destinations). Generous: snapshots can be large and links slow. The run
// is detached from the request/scheduler ctx (which only gates the local
// backup) so remote I/O never blocks the API or the scheduler tick.
const offsiteTimeout = 10 * time.Minute

// OffsiteAfterSnapshot is the BackupManager.AfterSnapshot implementation
// (P4): copy a successful snapshot to every enabled offsite destination,
// bundle the shares key when configured, prune to keep_n per destination,
// then audit + notify. Upload failures NEVER fail the local backup — they
// are logged, audited (backup.offsite_failed) and fanned out as operational
// alerts when any notification channel is configured. Exported so main can
// wire it onto the BackupManager after construction.
func (s *Server) OffsiteAfterSnapshot(ctx context.Context, info *BackupInfo) {
	st, err := s.Store.GetSettings(ctx)
	if err != nil {
		// Fail closed on config reads: an undecryptable settings row must
		// not silently skip offsite copies without a trace.
		s.Log.Error("offsite backup: settings unreadable, skipping", "err", err, "name", info.Name)
		s.auditSystem(ctx, "backup.offsite_failed", "db", map[string]any{
			"name": info.Name, "error": "settings unreadable",
		})
		return
	}
	if st == nil || (!st.BackupOffsite.S3.Enabled && !st.BackupOffsite.SFTP.Enabled) {
		return // offsite not configured — nothing to do
	}
	ctx, cancel := context.WithTimeout(ctxBg(), offsiteTimeout)
	defer cancel()

	build := s.OffsiteDestinations
	if build == nil {
		build = func(st *model.Settings) ([]offsite.Destination, []offsite.Result) {
			return offsite.BuildDestinations(ctx, st, nil)
		}
	}
	dests, results := build(st)
	runResults, _ := offsite.Run(ctx, s.Log, dests,
		filepath.Join(s.Backup.Dir, info.Name), s.Store.SharesKeyPath(),
		st.BackupOffsite.IncludeSharesKey, st.BackupOffsite.KeepN)
	results = append(results, runResults...)

	for _, r := range results {
		// Per-destination key status: did THIS copy get the key companion?
		keyBundled := false
		for _, u := range r.Uploaded {
			if strings.HasSuffix(u, offsite.KeySuffix) {
				keyBundled = true
			}
		}
		detail := map[string]any{
			"name":        info.Name,
			"destination": r.Destination,
			"uploaded":    r.Uploaded,
			// Summarize at the SOURCE: a large retention prune (hundreds of
			// names) would blow the 1024-byte audit detail cap and truncate
			// the key fields. Full list stays in the structured log below.
			"pruned":             summarizeAuditList(r.Pruned, auditListKeep),
			"shares_key_bundled": keyBundled,
		}
		if r.Err != "" {
			// Transport errors can embed credential-bearing URLs — render
			// through the same redaction the notification channels use.
			errText := notify.RedactURL(r.Err)
			detail["error"] = errText
			s.Log.Error("offsite backup failed", "destination", r.Destination, "name", info.Name, "err", errText)
			s.auditSystem(ctx, "backup.offsite_failed", "db", detail)
			s.offsiteAlert(ctx, st, fmt.Sprintf(
				"⚠️ **Offsite backup failed** — destination `%s`, snapshot `%s`\n%s",
				r.Destination, info.Name, errText))
			continue
		}
		if r.PruneErr != "" {
			detail["prune_error"] = notify.RedactURL(r.PruneErr)
			s.Log.Warn("offsite retention prune failed", "destination", r.Destination,
				"name", info.Name, "err", notify.RedactURL(r.PruneErr))
		}
		s.Log.Info("offsite backup uploaded", "destination", r.Destination, "name", info.Name,
			"objects", len(r.Uploaded), "pruned", len(r.Pruned), "shares_key", keyBundled)
		s.auditSystem(ctx, "backup.offsite", "db", detail)
	}
}

// offsiteAlert fans an operational alert out to the configured channels,
// mirroring the disk/node alert paths (Notifier override for tests, live
// settings fan-out otherwise). No channels configured = silent — the audit
// row and structured log remain the failure record.
func (s *Server) offsiteAlert(ctx context.Context, st *model.Settings, content string) {
	if !notify.AnyConfigured(st) {
		return
	}
	if s.Notifier != nil {
		s.Notifier.Alert(ctx, content)
		return
	}
	notify.NewFanoutFromSettings(st, st.DiscordWebhook, s.mailSender(), s.Log).Alert(ctx, content)
}
