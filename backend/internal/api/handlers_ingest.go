package api

import (
	"strings"

	"github.com/badskater/encode-system/backend/internal/ingest"
	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Experimental download ingestion (P11) ----------

// validateIngest checks the ingestion block. Shape bounds (interval,
// category-map folder names) apply even when disabled — the stored value
// must always be sane. Connection fields are required only when Enabled.
// Credential presence is checked post-merge in handleUpdateSettings (a
// blank password keeps the stored one), mirroring the SMTP/offsite rule.
func validateIngest(in *model.IngestSettings) error {
	in.Provider = strings.TrimSpace(in.Provider)
	in.URL = strings.TrimRight(strings.TrimSpace(in.URL), "/")
	in.Username = strings.TrimSpace(in.Username)
	in.Category = strings.TrimSpace(in.Category)
	in.DownloadRoot = strings.TrimSpace(in.DownloadRoot)
	// Cadence: 0 = default (60s); anything else must be 10s-1h. The poll
	// loop re-clamps to the same bounds, so a hand-edited row cannot spin.
	if in.PollIntervalSeconds < 0 || in.PollIntervalSeconds > 3600 ||
		(in.PollIntervalSeconds > 0 && in.PollIntervalSeconds < 10) {
		return errSettings("ingest.poll_interval_seconds must be 0 (default 60) or 10-3600")
	}
	// Category filter: a plain label, no control characters.
	if in.Category != "" && !ingest.ValidSeriesName(in.Category) {
		return errSettings("ingest.category must be a plain category name (no slashes, traversal or control characters)")
	}
	// Category→series map: normalize keys/values, validate every series
	// folder name NOW so a typo is caught at save time, not mid-staging.
	if len(in.CategoryMap) > 0 {
		normalized := make(map[string]string, len(in.CategoryMap))
		for k, v := range in.CategoryMap {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" || v == "" {
				return errSettings("ingest.category_map keys and values must be non-empty")
			}
			if !ingest.ValidSeriesName(k) {
				return errSettings("ingest.category_map keys must be plain category names (no slashes, traversal or control characters)")
			}
			if !ingest.ValidSeriesName(v) {
				return errSettings("ingest.category_map values must be valid series folder names (no slashes, traversal or control characters)")
			}
			if _, dup := normalized[k]; dup {
				return errSettings("ingest.category_map has duplicate keys after trimming")
			}
			normalized[k] = v
		}
		in.CategoryMap = normalized
	}
	if !in.Enabled {
		return nil // disabled: shape checks only
	}
	switch in.Provider {
	case model.IngestProviderQBit, model.IngestProviderRuTorrent:
	default:
		return errSettings("ingest.provider must be qbittorrent or rutorrent when ingestion is enabled")
	}
	if in.URL == "" || !isHTTPURL(in.URL) {
		return errSettings("ingest.url must be an http(s) URL when ingestion is enabled (e.g. http://192.168.1.10:8080)")
	}
	if in.DownloadRoot == "" {
		return errSettings("ingest.download_root is required when ingestion is enabled (the controller-visible mount of the client's download directory)")
	}
	if !isUnixPath(in.DownloadRoot) {
		return errSettings("ingest.download_root must be an absolute Unix path (e.g. /mnt/downloads)")
	}
	for _, seg := range strings.Split(in.DownloadRoot, "/") {
		if seg == ".." {
			return errSettings("ingest.download_root must not contain .. segments")
		}
	}
	return nil
}
