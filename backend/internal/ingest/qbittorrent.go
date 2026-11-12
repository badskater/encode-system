package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// QBit is a bounded qBittorrent WebAPI v2 client (first-class provider).
// Auth is the cookie session flow: POST /api/v2/auth/login returns a SID
// cookie; an expired or rejected session answers 403 (4.x–5.1) or 401
// (≥5.2 auth paths) and the client re-logs in once and retries the
// request. Every exchange is bounded by httpTimeout and the caller's
// context.
type QBit struct {
	base     string // normalized base URL, no trailing slash
	username string
	password string
	http     *http.Client
	log      *slog.Logger
	loggedIn bool
}

// NewQBit builds a client for the qBittorrent web UI at baseURL. An empty
// username means the instance has auth bypassed (e.g. localhost whitelist)
// — no login is attempted until a 403 proves otherwise.
func NewQBit(baseURL, username, password string, log *slog.Logger) (*QBit, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("qbittorrent: invalid URL %q", baseURL)
	}
	if log == nil {
		log = slog.Default()
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: cookie jar: %w", err)
	}
	return &QBit{
		base:     base,
		username: username,
		password: password,
		http:     newBoundedClient(jar),
		log:      log,
	}, nil
}

// login performs the WebAPI v2 cookie login across both protocol
// generations (verified against upstream qbittorrent/qBittorrent,
// src/webui/api/authcontroller.cpp + src/webui/webapplication.cpp):
//   - 4.x–5.1 (v4_6_x/v5_0_x/v5_1_x): 200 with body "Ok." on success,
//     "Fails." on bad credentials.
//   - ≥5.2 (v5_2_x/v5_3_x/master): success sets only the API status and
//     null result data renders as 204 (no body); bad credentials throw
//     APIErrorType::Unauthorized → 401.
//
// 403 on login means the IP is banned in both generations. The SID
// cookie is stored in the client's jar.
func (q *QBit) login(ctx context.Context) error {
	form := url.Values{}
	form.Set("username", q.username)
	form.Set("password", q.password)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		q.base+"/api/v2/auth/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := q.http.Do(req)
	if err != nil {
		return fmt.Errorf("qbittorrent login: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("qbittorrent login: read body: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("qbittorrent login: IP banned (403)")
	case resp.StatusCode == http.StatusUnauthorized:
		// ≥5.2: bad credentials are 401, not 200 "Fails.".
		return fmt.Errorf("qbittorrent login: authentication failed (401)")
	case resp.StatusCode == http.StatusNoContent:
		// ≥5.2: login success carries no body.
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("qbittorrent login: HTTP %d", resp.StatusCode)
	case strings.TrimSpace(string(body)) != "Ok.":
		// 4.x–5.1: "Fails." = bad credentials. Never echo the body
		// verbatim into logs beyond this fixed classification.
		return fmt.Errorf("qbittorrent login: authentication failed")
	}
	q.loggedIn = true
	return nil
}

// get runs an authenticated GET. On 401 or 403 (expired/rejected
// session) it re-logs in ONCE and retries; a second failure is a hard
// error. 403 is the no-session status on every generation (4.x–5.1 and
// the ≥5.2 private-endpoint check); 401 covers the ≥5.2 Unauthorized
// paths. With no credentials configured either status means auth is
// required but unavailable.
func (q *QBit) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if !q.loggedIn && q.username != "" {
		if err := q.login(ctx); err != nil {
			return nil, err
		}
	}
	body, status, err := q.doGet(ctx, path, query)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		if q.username == "" {
			return nil, fmt.Errorf("qbittorrent: %s: %d (auth required but no username configured)", path, status)
		}
		q.loggedIn = false
		if err := q.login(ctx); err != nil {
			return nil, err
		}
		body, status, err = q.doGet(ctx, path, query)
		if err != nil {
			return nil, err
		}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("qbittorrent: %s: HTTP %d", path, status)
	}
	return body, nil
}

// doGet is one bounded GET exchange (no retry logic).
func (q *QBit) doGet(ctx context.Context, path string, query url.Values) ([]byte, int, error) {
	u := q.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := q.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("qbittorrent: %s: %w", path, err)
	}
	defer resp.Body.Close()
	// Bound the read: a torrent list is small; a pathological response
	// must not exhaust memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("qbittorrent: %s: read body: %w", path, err)
	}
	return body, resp.StatusCode, nil
}

// qbitTorrent is the subset of /api/v2/torrents/info fields we consume.
type qbitTorrent struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	Category string `json:"category"`
	SavePath string `json:"save_path"`
}

// qbitFile is one /api/v2/torrents/files entry. Name is relative to the
// torrent save path.
type qbitFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Completed lists completed torrents (filter=completed), optionally in
// one category, and fetches each torrent's file list. A torrent whose
// file list cannot be fetched is skipped with a log (per-torrent
// isolation — it is retried on the next poll), never fatal to the batch.
func (q *QBit) Completed(ctx context.Context, category string) ([]Torrent, error) {
	query := url.Values{}
	query.Set("filter", "completed")
	if category != "" {
		query.Set("category", category)
	}
	body, err := q.get(ctx, "/api/v2/torrents/info", query)
	if err != nil {
		return nil, err
	}
	var list []qbitTorrent
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("qbittorrent: decode torrent list: %w", err)
	}
	var out []Torrent
	for _, t := range list {
		if t.Hash == "" || t.SavePath == "" {
			continue // malformed entry: skip, never guess
		}
		fq := url.Values{}
		fq.Set("hash", t.Hash)
		fb, err := q.get(ctx, "/api/v2/torrents/files", fq)
		if err != nil {
			q.log.Warn("ingest: torrent files fetch failed; skipping this poll",
				"provider", model.IngestProviderQBit, "hash", t.Hash, "err", err)
			continue
		}
		var files []qbitFile
		if err := json.Unmarshal(fb, &files); err != nil {
			q.log.Warn("ingest: torrent files decode failed; skipping this poll",
				"provider", model.IngestProviderQBit, "hash", t.Hash, "err", err)
			continue
		}
		tt := Torrent{Hash: t.Hash, Name: t.Name, Category: t.Category, SavePath: t.SavePath}
		for _, f := range files {
			if f.Name == "" {
				continue
			}
			tt.Files = append(tt.Files, File{Name: f.Name, Size: f.Size})
		}
		out = append(out, tt)
	}
	return out, nil
}
