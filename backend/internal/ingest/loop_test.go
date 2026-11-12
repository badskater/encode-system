package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/store"
)

// fakeClient is an in-memory Client for loop tests (the HTTP clients are
// tested separately against httptest mocks).
type fakeClient struct {
	mu      sync.Mutex
	byCat   map[string][]Torrent
	calls   []string
	failCat string // category whose Completed errors
}

func (f *fakeClient) Completed(ctx context.Context, category string) ([]Torrent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, category)
	if f.failCat != "" && category == f.failCat {
		return nil, fmt.Errorf("mock list failure")
	}
	return f.byCat[category], nil
}

// loopFixture wires a real SQLite store (the production ledger), a fake
// client, and a download root + scripts root on disk.
type loopFixture struct {
	st       *store.Store
	client   *fakeClient
	root     string // download root
	scripts  string // scripts root
	events   []StagedEvent
	settings model.IngestSettings
}

func newLoopFixture(t *testing.T) *loopFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &loopFixture{
		st:      st,
		client:  &fakeClient{byCat: map[string][]Torrent{}},
		root:    t.TempDir(),
		scripts: t.TempDir(),
		settings: model.IngestSettings{
			Enabled:      true,
			Provider:     model.IngestProviderQBit,
			URL:          "http://127.0.0.1:8080",
			DownloadRoot: "", // set below
		},
	}
	f.settings.DownloadRoot = f.root
	return f
}

func (f *loopFixture) config() Config {
	return Config{Settings: f.settings, ScriptsRoot: f.scripts}
}

func (f *loopFixture) factory(model.IngestSettings) (Client, error) { return f.client, nil }

func (f *loopFixture) onStaged(ev StagedEvent) { f.events = append(f.events, ev) }

func (f *loopFixture) poll(t *testing.T) {
	t.Helper()
	PollOnce(context.Background(), nil, f.st, f.config(), f.factory, f.onStaged)
}

// seedDownload creates a torrent folder with one video file under the
// download root and returns the matching Torrent.
func (f *loopFixture) seedDownload(t *testing.T, hash, name, category, dirName string) Torrent {
	t.Helper()
	dir := filepath.Join(f.root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.mkv"), []byte("video-"+hash), 0o644); err != nil {
		t.Fatal(err)
	}
	return Torrent{
		Hash: hash, Name: name, Category: category,
		SavePath: "/client/downloads/" + dirName,
		Files:    []File{{Name: "source.mkv", Size: 10}},
	}
}

// TestPollStagesAndDedupes proves the core flow: a completed torrent is
// staged into <series>/Ep NN, recorded in the ledger, and a second poll
// with the same torrent stages NOTHING (dedupe).
func TestPollStagesAndDedupes(t *testing.T) {
	f := newLoopFixture(t)
	tor := f.seedDownload(t, "hash1", "Some.Show.S01E01.1080p", "anime", "Some.Show.S01E01.1080p")
	f.client.byCat[""] = []Torrent{tor}

	f.poll(t)
	if len(f.events) != 1 {
		t.Fatalf("expected 1 staged event, got %d", len(f.events))
	}
	ev := f.events[0]
	if ev.Series != "Some Show" || ev.Episode != "01" || ev.StagedPath != filepath.Join("Some Show", "Ep 01") {
		t.Fatalf("event: %+v", ev)
	}
	src, err := os.ReadFile(filepath.Join(f.scripts, "Some Show", "Ep 01", "source.mkv"))
	if err != nil || string(src) != "video-hash1" {
		t.Fatalf("staged source: %q err=%v", src, err)
	}
	seen, err := f.st.IngestSeen(context.Background(), model.IngestProviderQBit, "hash1")
	if err != nil || !seen {
		t.Fatalf("ledger must record the torrent: seen=%v err=%v", seen, err)
	}

	// Second poll: same torrent still completed in the client → no re-stage.
	f.poll(t)
	if len(f.events) != 1 {
		t.Fatalf("dedupe failed: %d events after second poll", len(f.events))
	}
}

// TestLedgerSurvivesRestart proves the dedupe ledger is persistent: a
// fresh store handle on the same DB still sees the staged torrent.
func TestLedgerSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st1.RecordIngest(ctx, "qbittorrent", "h-restart", "Name", "Series", "01", "Series/Ep 01"); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	seen, err := st2.IngestSeen(ctx, "qbittorrent", "h-restart")
	if err != nil || !seen {
		t.Fatalf("ledger must survive restart: seen=%v err=%v", seen, err)
	}
}

// TestPollCategoryMapOverride proves the category→series map overrides
// the parsed series name while the episode still comes from the name.
func TestPollCategoryMapOverride(t *testing.T) {
	f := newLoopFixture(t)
	f.settings.CategoryMap = map[string]string{"anime": "Canonical Show"}
	tor := f.seedDownload(t, "h2", "Wrong.Name.S01E05.1080p", "anime", "Wrong.Name.S01E05")
	f.client.byCat["anime"] = []Torrent{tor}

	f.poll(t)
	if len(f.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(f.events))
	}
	if f.events[0].Series != "Canonical Show" || f.events[0].Episode != "05" {
		t.Fatalf("map override: %+v", f.events[0])
	}
	// Only the mapped category was polled.
	if len(f.client.calls) != 1 || f.client.calls[0] != "anime" {
		t.Fatalf("map keys must drive polling: %v", f.client.calls)
	}
}

// TestPollCategoryFilter proves the explicit category filter wins over
// the map and restricts the client query.
func TestPollCategoryFilter(t *testing.T) {
	f := newLoopFixture(t)
	f.settings.Category = "only-this"
	f.settings.CategoryMap = map[string]string{"other": "X"}
	f.client.byCat["only-this"] = nil
	f.poll(t)
	if len(f.client.calls) != 1 || f.client.calls[0] != "only-this" {
		t.Fatalf("explicit category must win: %v", f.client.calls)
	}
}

// TestPollUnparseableSkipped proves an unparseable release name is
// skipped (never guessed) and does not block other torrents.
func TestPollUnparseableSkipped(t *testing.T) {
	f := newLoopFixture(t)
	bad := f.seedDownload(t, "bad", "Totally.Ambiguous.Release.1080p", "", "bad")
	good := f.seedDownload(t, "good", "Good.Show.S01E02", "", "good")
	f.client.byCat[""] = []Torrent{bad, good}
	f.poll(t)
	if len(f.events) != 1 || f.events[0].Hash != "good" {
		t.Fatalf("unparseable must skip, good must stage: %+v", f.events)
	}
	seen, _ := f.st.IngestSeen(context.Background(), model.IngestProviderQBit, "bad")
	if seen {
		t.Fatal("skipped torrent must not enter the ledger")
	}
}

// TestPollMappedButUnparseableEpisodeSkipped proves a mapped category
// still requires a parseable EPISODE — the map never supplies one.
func TestPollMappedButUnparseableEpisodeSkipped(t *testing.T) {
	f := newLoopFixture(t)
	f.settings.CategoryMap = map[string]string{"anime": "Mapped"}
	tor := f.seedDownload(t, "h3", "No.Episode.Signal.1080p", "anime", "h3dir")
	f.client.byCat["anime"] = []Torrent{tor}
	f.poll(t)
	if len(f.events) != 0 {
		t.Fatalf("mapped category with unparseable episode must skip: %+v", f.events)
	}
}

// TestPollPerTorrentErrorIsolation proves one torrent's staging failure
// (missing dir) does not stop the others.
func TestPollPerTorrentErrorIsolation(t *testing.T) {
	f := newLoopFixture(t)
	missing := Torrent{Hash: "missing", Name: "Missing.Show.S01E01", SavePath: "/d/absent-dir",
		Files: []File{{Name: "a.mkv"}}}
	good := f.seedDownload(t, "good", "Good.Show.S01E03", "", "good")
	f.client.byCat[""] = []Torrent{missing, good}
	f.poll(t)
	if len(f.events) != 1 || f.events[0].Hash != "good" {
		t.Fatalf("bad torrent must not block the batch: %+v", f.events)
	}
}

// TestPollCategoryListErrorIsolation proves one category's list failure
// does not stop the other categories.
func TestPollCategoryListErrorIsolation(t *testing.T) {
	f := newLoopFixture(t)
	f.settings.CategoryMap = map[string]string{"broken": "B", "working": "W"}
	f.client.failCat = "broken"
	good := f.seedDownload(t, "gw", "Show.S01E01", "working", "gwdir")
	f.client.byCat["working"] = []Torrent{good}
	f.poll(t)
	if len(f.events) != 1 || f.events[0].Series != "W" {
		t.Fatalf("broken category must not block working one: %+v", f.events)
	}
}

// TestPollDisabledNoop proves a disabled config never touches the client.
func TestPollDisabledNoop(t *testing.T) {
	f := newLoopFixture(t)
	f.settings.Enabled = false
	f.client.byCat[""] = []Torrent{{Hash: "h", Name: "Show S01E01", SavePath: "/d/x"}}
	f.poll(t)
	if len(f.client.calls) != 0 || len(f.events) != 0 {
		t.Fatalf("disabled ingest must be a no-op: calls=%v events=%v", f.client.calls, f.events)
	}
}

// TestPollMultiSeasonRefused proves S02+ releases are skipped end to end
// (the parser refuses them; the loop must not stage them).
func TestPollMultiSeasonRefused(t *testing.T) {
	f := newLoopFixture(t)
	tor := f.seedDownload(t, "s2", "Show.S02E01.1080p", "", "s2dir")
	f.client.byCat[""] = []Torrent{tor}
	f.poll(t)
	if len(f.events) != 0 {
		t.Fatalf("multi-season must be refused: %+v", f.events)
	}
}

// TestPollIntervalBounds proves the cadence resolution mirrors the API
// validation bounds.
func TestPollIntervalBounds(t *testing.T) {
	cases := []struct {
		seconds int
		want    string
	}{
		{0, "1m0s"}, {60, "1m0s"}, {10, "10s"}, {3600, "1h0m0s"},
		{5, "1m0s"}, {7200, "1m0s"}, {-3, "1m0s"},
	}
	for _, tc := range cases {
		if got := pollInterval(tc.seconds).String(); got != tc.want {
			t.Errorf("pollInterval(%d) = %s, want %s", tc.seconds, got, tc.want)
		}
	}
}
