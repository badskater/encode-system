package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeRuTorrent mocks the httprpc plugin contract grounded in the real
// plugins/httprpc/action.php: form-encoded mode=list (fixed 35-column
// d.multicall layout keyed by hash, appended cmd columns last) and
// mode=fls file rows. It asserts the client sends the d.get_complete
// append and HTTP Basic credentials.
type fakeRuTorrent struct {
	srv         *httptest.Server
	user        string
	pass        string
	listCalls   atomic.Int64
	flsCalls    atomic.Int64
	sawAppend   atomic.Bool
	sawBasic    atomic.Bool
	torrents    map[string][]any
	filesByHash map[string][][]any
	filesFail   map[string]bool
}

// rtRow builds one 35-column list row (indices per rutorrent.go). Only
// the cells the client reads are populated; the rest are zero strings.
func rtRow(name, label, basePath string, multiFile, complete bool) []any {
	row := make([]any, 36)
	for i := range row {
		row[i] = "0"
	}
	row[rtColName] = name
	row[rtColCustom1] = label
	row[rtColBasePath] = basePath
	if multiFile {
		row[rtColIsMultiFile] = "1"
	}
	if complete {
		row[rtColComplete] = "1"
	}
	row[rtColLeftBytes] = "0"
	return row
}

func newFakeRuTorrent(t *testing.T) *fakeRuTorrent {
	t.Helper()
	f := &fakeRuTorrent{
		user:        "rt-" + "user",
		pass:        "rt-" + "pass-456",
		torrents:    map[string][]any{},
		filesByHash: map[string][][]any{},
		filesFail:   map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plugins/httprpc/action.php" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if u, p, ok := r.BasicAuth(); ok && u == f.user && p == f.pass {
			f.sawBasic.Store(true)
		} else if f.user != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.ParseForm()
		switch r.FormValue("mode") {
		case "list":
			f.listCalls.Add(1)
			for _, cmd := range r.Form["cmd"] {
				if cmd == "d.get_complete=" {
					f.sawAppend.Store(true)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"t": f.torrents, "cid": 1})
		case "fls":
			f.flsCalls.Add(1)
			hash := r.FormValue("hash")
			if f.filesFail[hash] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			rows := f.filesByHash[hash]
			if rows == nil {
				rows = [][]any{}
			}
			json.NewEncoder(w).Encode(rows)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRuTorrent) client(t *testing.T) *RuTorrent {
	t.Helper()
	c, err := NewRuTorrent(f.srv.URL, f.user, f.pass, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestRuTorrentCompleted proves the list flow: the d.get_complete append
// travels, Basic auth is attached, completed torrents are returned with
// their file lists, and multi-file vs single-file save paths resolve.
func TestRuTorrentCompleted(t *testing.T) {
	f := newFakeRuTorrent(t)
	f.torrents["hash1"] = rtRow("Show.S01E01.1080p", "anime", "/downloads/Show.S01E01.1080p", true, true)
	f.torrents["hash2"] = rtRow("Movie.2024.1080p", "anime", "/downloads/movie.mkv", false, true)
	f.torrents["hash3"] = rtRow("Incomplete.S01E02", "anime", "/downloads/inc", true, false)
	f.filesByHash["hash1"] = [][]any{{"show.s01e01.mkv", "10", "10", 1000, "1"}}
	f.filesByHash["hash2"] = [][]any{{"movie.mkv", "10", "10", 2000, "1"}}

	c := f.client(t)
	got, err := c.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("incomplete torrent must be filtered, got %+v", got)
	}
	byHash := map[string]Torrent{}
	for _, tt := range got {
		byHash[tt.Hash] = tt
	}
	multi := byHash["hash1"]
	if multi.SavePath != "/downloads/Show.S01E01.1080p" || multi.Category != "anime" ||
		len(multi.Files) != 1 || multi.Files[0].Name != "show.s01e01.mkv" || multi.Files[0].Size != 1000 {
		t.Fatalf("multi-file torrent: %+v", multi)
	}
	single := byHash["hash2"]
	if single.SavePath != "/downloads" {
		t.Fatalf("single-file save path must be the parent dir, got %q", single.SavePath)
	}
	if !f.sawAppend.Load() {
		t.Fatal("client must append cmd=d.get_complete=")
	}
	if !f.sawBasic.Load() {
		t.Fatal("client must send HTTP Basic credentials")
	}
}

// TestRuTorrentCategoryFilter proves the label (custom1) filter.
func TestRuTorrentCategoryFilter(t *testing.T) {
	f := newFakeRuTorrent(t)
	f.torrents["h1"] = rtRow("A S01E01", "anime", "/d/a", true, true)
	f.torrents["h2"] = rtRow("B S01E01", "other", "/d/b", true, true)
	c := f.client(t)
	got, err := c.Completed(context.Background(), "anime")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hash != "h1" {
		t.Fatalf("label filter: got %+v", got)
	}
}

// TestRuTorrentLeftBytesFallback proves the best-effort completion
// fallback when the plugin drops the appended command (35-column rows).
func TestRuTorrentLeftBytesFallback(t *testing.T) {
	f := newFakeRuTorrent(t)
	row := rtRow("A S01E01", "", "/d/a", true, false)[:35] // no append column
	row[rtColLeftBytes] = "0"                              // fully downloaded
	f.torrents["h1"] = row
	row2 := rtRow("B S01E01", "", "/d/b", true, false)[:35]
	row2[rtColLeftBytes] = "500" // still downloading
	f.torrents["h2"] = row2
	c := f.client(t)
	got, err := c.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hash != "h1" {
		t.Fatalf("left_bytes fallback: got %+v", got)
	}
}

// TestRuTorrentNumericCells proves numeric JSON cells (PHP encodes ints
// as numbers) normalize correctly.
func TestRuTorrentNumericCells(t *testing.T) {
	f := newFakeRuTorrent(t)
	row := rtRow("A S01E01", "anime", "/d/a", true, true)
	row[rtColComplete] = 1 // number, not string
	row[rtColIsMultiFile] = 1
	f.torrents["h1"] = row
	f.filesByHash["h1"] = [][]any{{"a.mkv", 10, 10, 1234, 1}}
	c := f.client(t)
	got, err := c.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Files[0].Size != 1234 {
		t.Fatalf("numeric cells: got %+v", got)
	}
}

// TestRuTorrentFilesIsolation proves one broken file list skips only
// that torrent.
func TestRuTorrentFilesIsolation(t *testing.T) {
	f := newFakeRuTorrent(t)
	f.torrents["good"] = rtRow("Good S01E01", "", "/d/good", true, true)
	f.torrents["bad"] = rtRow("Bad S01E01", "", "/d/bad", true, true)
	f.filesByHash["good"] = [][]any{{"g.mkv", "1", "1", 1, "1"}}
	f.filesFail["bad"] = true
	c := f.client(t)
	got, err := c.Completed(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Hash != "good" {
		t.Fatalf("bad torrent must be skipped: %+v", got)
	}
}

// TestRuTorrentAuthFailure proves a 401 is a hard error.
func TestRuTorrentAuthFailure(t *testing.T) {
	f := newFakeRuTorrent(t)
	c, err := NewRuTorrent(f.srv.URL, "wrong-"+"user", "wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Completed(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad basic auth must error, got %v", err)
	}
}

// TestRuTorrentBoundedClient proves the explicit timeout + ctx cancel.
func TestRuTorrentBoundedClient(t *testing.T) {
	f := newFakeRuTorrent(t)
	c := f.client(t)
	if c.http.Timeout != httpTimeout || c.http == http.DefaultClient {
		t.Fatal("client must be bounded and not the default client")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Completed(ctx, ""); err == nil {
		t.Fatal("cancelled ctx must abort")
	}
}

// TestRuTorrentInvalidURL proves construction rejects bad targets.
func TestRuTorrentInvalidURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x", "nope"} {
		if _, err := NewRuTorrent(u, "", "", nil); err == nil {
			t.Fatalf("NewRuTorrent(%q) must fail", u)
		}
	}
}

// TestRuSingleFileStagesEndToEnd pins Q5 (ruTorrent side): is_multi_file=0
// → savePath = Dir(basePath) = the download ROOT; the file sits at
// downloadRoot/<filename> and must stage end to end (mock → Stage).
func TestRuSingleFileStagesEndToEnd(t *testing.T) {
	f := newFakeRuTorrent(t)
	f.torrents["h1"] = rtRow("Movie.2024.1080p", "", "/downloads/movie.mkv", false, true)
	f.filesByHash["h1"] = [][]any{{"movie.mkv", "1", "1", 5, "1"}}
	c := f.client(t)
	got, err := c.Completed(context.Background(), "")
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v %d", err, len(got))
	}
	if got[0].SavePath != "/downloads" {
		t.Fatalf("single-file savePath must be the parent dir, got %q", got[0].SavePath)
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

// TestRuFilesFetchErrorRedacted pins Q4 (client log site): a transport
// failure on the per-torrent file list embeds the FULL request URL — the
// skip-log must render it through notify.RedactURL.
func TestRuFilesFetchErrorRedacted(t *testing.T) {
	secret := "leak-" + "canary-99"
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("mode") == "fls" {
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
			return
		}
		row := rtRow("Show S01E01", "", "/d/Show", true, true)
		json.NewEncoder(w).Encode(map[string]any{"t": map[string]any{"h1": row}})
	}))
	defer srv.Close()
	credURL := strings.Replace(srv.URL, "http://", "http://probeuser:"+secret+"@", 1)
	c, err := NewRuTorrent(credURL, "", "", log)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Completed(context.Background(), "")
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
