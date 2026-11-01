package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// githubRelease is the subset of the GitHub releases/latest response we use.
type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// handleUpdateCheck: GET /api/updates/check — compares the running controller
// version to the latest GitHub release tag.
//
// Calls {api}/repos/{repo}/releases/latest with a 10s timeout,
// Accept: application/vnd.github+json, User-Agent: encode-system, and a
// Bearer token when ENCODE_GITHUB_TOKEN is set. Returns 200 with the
// comparison result, or 502 on any upstream failure.
//
// Tag comparison strips a leading "v" and compares dot-separated numeric
// segments (1.12.3 > 1.9.0). Non-numeric segments fall back to string
// comparison so a tag like "1.0.0-rc1" degrades gracefully.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	current := s.Cfg.Version
	repo := s.Cfg.GitHubRepo
	if repo == "" {
		repo = "badskater/encode-system"
	}
	apiBase := s.Cfg.GitHubAPI
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimRight(apiBase, "/"), repo)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "update check failed: build request: "+err.Error())
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "encode-system")
	if s.Cfg.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.Cfg.GitHubToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "update check failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeErr(w, http.StatusBadGateway, "update check failed: read body: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("update check failed: GitHub returned %d", resp.StatusCode))
		return
	}
	var rel githubRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		writeErr(w, http.StatusBadGateway, "update check failed: malformed JSON: "+err.Error())
		return
	}
	latest := rel.TagName
	available := compareVersions(current, latest) < 0
	writeJSON(w, http.StatusOK, map[string]any{
		"current":          current,
		"latest":           latest,
		"update_available": available,
		"release_url":      rel.HTMLURL,
	})
}

// compareVersions compares two version strings as dot-separated numeric
// segments. Returns -1 if a < b, 0 if equal, 1 if a > b. A leading "v" is
// stripped. Non-numeric segments fall back to string comparison so tags
// like "1.0.0-rc1" don't crash.
func compareVersions(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	if a == "" {
		// Empty current version: any non-empty latest is "newer".
		if b == "" {
			return 0
		}
		return -1
	}
	if b == "" {
		return 1
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var ap, bp string
		if i < len(as) {
			ap = as[i]
		}
		if i < len(bs) {
			bp = bs[i]
		}
		// Try numeric comparison first.
		ai, aok := atoiSafe(ap)
		bi, bok := atoiSafe(bp)
		if aok && bok {
			if ai < bi {
				return -1
			}
			if ai > bi {
				return 1
			}
			continue
		}
		// Fallback: string comparison for non-numeric segments.
		if ap < bp {
			return -1
		}
		if ap > bp {
			return 1
		}
	}
	return 0
}

// atoiSafe parses a decimal integer from a string, returning (value, true)
// on success. Returns (0, false) if the string is empty or contains any
// non-digit character — so "0" is valid but "0rc1" is not (string fallback).
func atoiSafe(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}
