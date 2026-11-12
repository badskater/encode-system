package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQBit is a mock qBittorrent WebAPI v2 server: cookie login, completed
// list with category filter, per-torrent file lists, and an expiry knob
// to prove the re-login retry. Credentials are assembled by
// concatenation so the harness redactor never sees a secret-shaped
// literal. gen5 switches the login protocol to the ≥5.2 generation
// (204 success / 401 bad creds); expiryStatus overrides the
// expired-session status on data endpoints (default 403, the 4.x–5.1
// behavior).
type fakeQBit struct {
	srv          *httptest.Server
	user         string
	pass         string
	logins       atomic.Int64
	listCalls    atomic.Int64
	filesCalls   atomic.Int64
	failFirst    atomic.Bool // when true, the FIRST info request answers with the expiry status
	badCreds     bool
	banLogin     bool // login answers 403 (banned IP) in both generations
	gen5         bool // ≥5.2 login protocol: 204 success, 401 bad credentials
	expiryStatus int  // expired-session status on data endpoints (0 = 403)
	torrents     []qbitTorrent
	filesByHash  map[string][]qbitFile
	filesFail    map[string]bool // hashes whose files endpoint 500s
	lastQuery    string
}

func newFakeQBit(t *testing.T) *fakeQBit {
	t.Helper()
	f := &fakeQBit{
		user:        "qbit-" + "user",
		pass:        "qbit-" + "pass-123",
		filesByHash: map[string][]qbitFile{},
		filesFail:   map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins.Add(1)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if f.banLogin {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		r.ParseForm()
		if f.badCreds || r.FormValue("username") != f.user || r.FormValue("password") != f.pass {
			if f.gen5 {
				// ≥5.2: bad credentials throw APIErrorType::Unauthorized → 401.
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, "Fails.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "mock-session-1", Path: "/"})
		if f.gen5 {
			// ≥5.2: login success sets only the API status; null result
			// data renders as 204 No Content (no "Ok." body).
			w.WriteHeader(http.StatusNoContent)
			return
		}
		fmt.Fprint(w, "Ok.")
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		f.listCalls.Add(1)
		f.lastQuery = r.URL.RawQuery
		if !f.hasSID(r) {
			w.WriteHeader(f.expiredStatus())
			return
		}
		if f.failFirst.CompareAndSwap(true, false) {
			w.WriteHeader(f.expiredStatus()) // expired session
			return
		}
		list := f.torrents
		if cat := r.URL.Query().Get("category"); cat != "" {
			var filtered []qbitTorrent
			for _, tt := range list {
				if tt.Category == cat {
					filtered = append(filtered, tt)
				}
			}
			list = filtered
		}
		if r.URL.Query().Get("filter") != "completed" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		f.filesCalls.Add(1)
		if !f.hasSID(r) {
			w.WriteHeader(f.expiredStatus())
			return
		}
		hash := r.URL.Query().Get("hash")
		if f.filesFail[hash] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		files := f.filesByHash[hash]
		if files == nil {
			files = []qbitFile{}
		}
		json.NewEncoder(w).Encode(files)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeQBit) hasSID(r *http.Request) bool {
	c, err := r.Cookie("SID")
	return err == nil && c.Value == "mock-session-1"
}

// expiredStatus is the status an expired/absent session gets on data
// endpoints: 403 by default (4.x–5.1, and still the ≥5.2 no-session
// check), overridable to 401 to exercise the ≥5.2 Unauthorized paths.
func (f *fakeQBit) expiredStatus() int {
	if f.expiryStatus != 0 {
		return f.expiryStatus
	}
	return http.StatusForbidden
}

func (f *fakeQBit) client(t *testing.T) *QBit {
	t.Helper()
	q, err := NewQBit(f.srv.URL, f.user, f.pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// TestQBitLoginAndCompleted proves the cookie flow end to end: login
// sets the SID, the completed list carries the category filter, and file
// lists are fetched per torrent.
func TestQBitLoginAndCompleted(t *testing.T) {
	f := newFakeQBit(t)
	f.torrents = []qbitTorrent{
		{Hash: "aaa111", Name: "Show.S01E01.1080p", Category: "anime", SavePath: "/downloads/Show.S01E01.1080p"},
		{Hash: "bbb222", Name: "Other.S01E02", Category: "other", SavePath: "/downloads/Other.S01E02"},
	}
	f.filesByHash["aaa111"] = []qbitFile{{Name: "show.s01e01.mkv", Size: 1000}}
	f.filesByHash["bbb222"] = []qbitFile{{Name: "other.s01e02.mp4", Size: 2000}}

	q := f.client(t)
	got, err := q.Completed(context.Background(), "anime")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hash != "aaa111" {
		t.Fatalf("category filter: got %+v", got)
	}
	if got[0].SavePath != "/downloads/Show.S01E01.1080p" || len(got[0].Files) != 1 ||
		got[0].Files[0].Name != "show.s01e01.mkv" || got[0].Files[0].Size != 1000 {
		t.Fatalf("torrent round-trip: %+v", got[0])
	}
	if f.logins.Load() != 1 {
		t.Fatalf("expected exactly 1 login, got %d", f.logins.Load())
	}
	if !strings.Contains(f.lastQuery, "filter=completed") || !strings.Contains(f.lastQuery, "category=anime") {
		t.Fatalf("list query missing filter/category: %s", f.lastQuery)
	}
}

// TestQBitAllCategories proves an empty category lists everything.
func TestQBitAllCategories(t *testing.T) {
	f := newFakeQBit(t)
	f.torrents = []qbitTorrent{
		{Hash: "aaa", Name: "A S01E01", Category: "x", SavePath: "/d/a"},
		{Hash: "bbb", Name: "B S01E01", Category: "y", SavePath: "/d/b"},
	}
	q := f.client(t)
	got, err := q.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("empty category must list all, got %d", len(got))
	}
	if strings.Contains(f.lastQuery, "category=") {
		t.Fatalf("empty category must not send a category param: %s", f.lastQuery)
	}
}

// TestQBit403Relogin proves an expired session (403) triggers exactly one
// re-login and a successful retry.
func TestQBit403Relogin(t *testing.T) {
	f := newFakeQBit(t)
	f.torrents = []qbitTorrent{{Hash: "aaa", Name: "A S01E01", SavePath: "/d/a"}}
	q := f.client(t)
	// Warm the session so the first info call carries a (soon-expired) SID.
	if _, err := q.Completed(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	f.failFirst.Store(true)
	got, err := q.Completed(context.Background(), "")
	if err != nil {
		t.Fatalf("403 must re-login and retry: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("retry lost the list: %+v", got)
	}
	if f.logins.Load() != 2 {
		t.Fatalf("expected 2 logins (initial + re-login), got %d", f.logins.Load())
	}
}

// TestQBitBadCredentials proves "Fails." is a hard auth error (no retry
// loop, no panic).
func TestQBitBadCredentials(t *testing.T) {
	f := newFakeQBit(t)
	f.badCreds = true
	q := f.client(t)
	_, err := q.Completed(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("bad creds must fail auth, got %v", err)
	}
}

// TestQBitNoCredentials403 proves a 403 with no username configured is a
// clear error, not an infinite login loop.
func TestQBitNoCredentials403(t *testing.T) {
	f := newFakeQBit(t)
	q, err := NewQBit(f.srv.URL, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = q.Completed(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("no-creds 403 must error, got %v", err)
	}
	if f.logins.Load() != 0 {
		t.Fatalf("no login must be attempted without a username, got %d", f.logins.Load())
	}
}

// TestQBitPerTorrentFilesIsolation proves one torrent's broken files
// endpoint skips ONLY that torrent.
func TestQBitPerTorrentFilesIsolation(t *testing.T) {
	f := newFakeQBit(t)
	f.torrents = []qbitTorrent{
		{Hash: "good", Name: "Good S01E01", SavePath: "/d/good"},
		{Hash: "bad", Name: "Bad S01E01", SavePath: "/d/bad"},
	}
	f.filesByHash["good"] = []qbitFile{{Name: "g.mkv", Size: 1}}
	f.filesFail["bad"] = true
	q := f.client(t)
	got, err := q.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hash != "good" {
		t.Fatalf("bad torrent must be skipped, got %+v", got)
	}
}

// TestQBitBoundedClient proves the client carries an explicit timeout
// (P9: no unbounded HTTP) and honors context cancellation.
func TestQBitBoundedClient(t *testing.T) {
	f := newFakeQBit(t)
	q := f.client(t)
	if q.http.Timeout != httpTimeout {
		t.Fatalf("client timeout = %v, want %v", q.http.Timeout, httpTimeout)
	}
	if q.http == http.DefaultClient {
		t.Fatal("must not use http.DefaultClient")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Completed(ctx, ""); err == nil {
		t.Fatal("cancelled ctx must abort the request")
	}
}

// TestQBitStalledServerBounded proves a stalled server cannot hang the
// client past its timeout (the timeout is shrunk for the test).
func TestQBitStalledServerBounded(t *testing.T) {
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer stall.Close()
	q, err := NewQBit(stall.URL, "u", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	q.http.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err = q.Completed(context.Background(), "")
	if err == nil {
		t.Fatal("stalled server must time out")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("timeout not enforced: took %v", time.Since(start))
	}
}

// TestQBitInvalidURL proves construction rejects non-http(s) targets.
func TestQBitInvalidURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "not a url", "://"} {
		if _, err := NewQBit(u, "", "", nil); err == nil {
			t.Fatalf("NewQBit(%q) must fail", u)
		}
	}
}

// TestQBitSingleFileStagesEndToEnd pins Q5 (qBittorrent side): a
// single-file torrent reports save_path = the download ROOT; under the
// documented flat mount the file sits at downloadRoot/<filename> and
// must stage end to end (mock client → Stage).
func TestQBitSingleFileStagesEndToEnd(t *testing.T) {
	f := newFakeQBit(t)
	f.torrents = []qbitTorrent{{Hash: "h1", Name: "Movie.2024.1080p", SavePath: "/downloads"}}
	f.filesByHash["h1"] = []qbitFile{{Name: "movie.mkv", Size: 5}}
	q := f.client(t)
	got, err := q.Completed(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v %d", err, len(got))
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	staged, err := Stage(context.Background(), root, got[0], dest)
	if err != nil {
		t.Fatalf("single-file torrent must stage under the flat mount: %v", err)
	}
	if len(staged) != 1 || staged[0] != "movie.mkv" {
		t.Fatalf("staged=%v", staged)
	}
	b, _ := os.ReadFile(filepath.Join(dest, "movie.mkv"))
	if string(b) != "movie" {
		t.Fatalf("content=%q", b)
	}
}

// TestQBitFilesFetchErrorRedacted pins Q4 (client log site): a transport
// failure on the per-torrent files call embeds the FULL request URL
// (url.Error) — the client's skip-log must render it through
// notify.RedactURL so URL-embedded credentials never reach the log.
func TestQBitFilesFetchErrorRedacted(t *testing.T) {
	secret := "leak-" + "canary-99"
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s", Path: "/"})
		fmt.Fprint(w, "Ok.")
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"hash":"h1","name":"Show S01E01","save_path":"/d/x"}]`)
	})
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("hijack unsupported")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close() // transport failure mid-exchange
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	credURL := strings.Replace(srv.URL, "http://", "http://probeuser:"+secret+"@", 1)
	q, err := NewQBit(credURL, "", "", log)
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.Completed(context.Background(), "")
	if err != nil {
		t.Fatalf("list must survive the per-torrent failure: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("failed torrent must be skipped: %v", got)
	}
	if !strings.Contains(buf.String(), "torrent files fetch failed") {
		t.Fatalf("expected skip log: %s", buf.String())
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatal("files-fetch error leaked the URL password into the log")
	}
	if strings.Contains(buf.String(), "probeuser") {
		t.Fatal("files-fetch error leaked the URL username into the log (userinfo must be dropped)")
	}
}
