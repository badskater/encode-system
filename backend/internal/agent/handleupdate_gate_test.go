package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestHandleUpdateSkipsLibBinOnLinux is ISSUE 2: on a Linux host the
// shouldSyncLibBin(runtime.GOOS) gate must keep handleUpdate from issuing
// /api/agent/download/lib and /download/bin requests even when the manifest
// bumps those versions — they are Windows-only artifacts. At the same time,
// when the agent version differs, the agent-binary sync path must still run
// (the controller serves a platform-scoped Linux ELF).
//
// runtime.GOOS is "linux" on this test host, so no build tags are needed.
// The test is skipped on non-Linux hosts (Windows would exercise the
// opposite path — lib/bin sync — and is covered by the syncLib/syncBin tests).
func TestHandleUpdateSkipsLibBinOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("gate behavior is linux-specific; run on a linux host")
	}
	// httptest serves plain HTTP; updates over HTTP need the explicit
	// trusted-LAN opt-in (getAuth refuses otherwise).
	t.Setenv("ENCODE_AGENT_ALLOW_HTTP_UPDATES", "1")

	// Fake controller: count every download path hit. Serve a small body
	// for /download/agent whose sha will NOT match the manifest so
	// syncAgent stops at the checksum check (no os.Executable staging, no
	// swap sidecar) — keeps the test deterministic and side-effect-free.
	var libHits, binHits, agentHits atomic.Int32
	agentPayload := []byte("not-the-linux-elf")
	agentSum := sha256.Sum256(agentPayload)
	agentWantSha := hex.EncodeToString(agentSum[:])
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/download/lib"):
			libHits.Add(1)
			w.Write([]byte("lib-payload"))
		case strings.HasSuffix(r.URL.Path, "/download/bin"):
			binHits.Add(1)
			w.Write([]byte("bin-payload"))
		case strings.HasSuffix(r.URL.Path, "/download/agent"):
			agentHits.Add(1)
			w.Write(agentPayload)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer controller.Close()

	dir := t.TempDir()
	cfg := Config{
		ControllerURL: controller.URL,
		NodeName:      "linux-node",
		Token:         nodeTok(),
		DataDir:       dir,
		LibPath:       filepath.Join(dir, "EncodeLib.ps1"),
	}
	a, err := New(cfg, "1.0.0", testLog())
	if err != nil {
		t.Fatal(err)
	}

	// Case 1: manifest bumps lib+bin but agent version MATCHES. On Linux
	// the gate skips lib/bin sync; syncAgent sees a matching version and
	// is a no-op. Zero download hits of any kind.
	m1 := model.UpdateManifest{
		AgentVersion: "1.0.0", // matches a.Version → syncAgent returns early
		AgentSHA256:  agentWantSha,
		LibVersion:   5, // bumped
		LibSHA256:    strings.Repeat("a", 64),
		BinVersion:   7, // bumped
		BinSHA256:    strings.Repeat("b", 64),
	}
	a.handleUpdate(m1)
	if got := libHits.Load(); got != 0 {
		t.Fatalf("case1 lib download hits = %d, want 0 (linux gate must skip lib)", got)
	}
	if got := binHits.Load(); got != 0 {
		t.Fatalf("case1 bin download hits = %d, want 0 (linux gate must skip bin)", got)
	}
	if got := agentHits.Load(); got != 0 {
		t.Fatalf("case1 agent download hits = %d, want 0 (version matches → no sync)", got)
	}

	// Case 2: agent version DIFFERS. The gate still skips lib/bin, but
	// syncAgent must run — it downloads the agent payload (the controller
	// serves the platform-scoped ELF) and stops at the checksum check
	// because the served sha != manifest sha. Lib/bin stay at zero hits;
	// the agent download fires exactly once.
	m2 := model.UpdateManifest{
		AgentVersion: "2.0.0", // differs → syncAgent runs
		// Deliberately WRONG sha: syncAgent downloads, checksums, and
		// refuses (returns before os.Executable / staging / swap).
		AgentSHA256: strings.Repeat("0", 64),
		LibVersion:  5,
		LibSHA256:   strings.Repeat("a", 64),
		BinVersion:  7,
		BinSHA256:   strings.Repeat("b", 64),
	}
	a.handleUpdate(m2)
	if got := libHits.Load(); got != 0 {
		t.Fatalf("case2 lib download hits = %d, want 0", got)
	}
	if got := binHits.Load(); got != 0 {
		t.Fatalf("case2 bin download hits = %d, want 0", got)
	}
	if got := agentHits.Load(); got != 1 {
		t.Fatalf("case2 agent download hits = %d, want 1 (agent sync must proceed when version differs)", got)
	}
}

// TestShouldSyncLibBinPlatformTable is a fast unit test for the gate helper
// itself: true only on windows, false on every other OS (linux, darwin, …).
// This pins the gate so a future refactor cannot silently widen lib/bin sync
// to Linux nodes (which would re-introduce the Windows-only-artifacts-on-
// Linux bug).
func TestShouldSyncLibBinPlatformTable(t *testing.T) {
	cases := []struct {
		goos string
		want bool
	}{
		{"windows", true},
		{"linux", false},
		{"darwin", false},
		{"freebsd", false},
		{"", false},
	}
	for _, c := range cases {
		if got := shouldSyncLibBin(c.goos); got != c.want {
			t.Errorf("shouldSyncLibBin(%q) = %v, want %v", c.goos, got, c.want)
		}
	}
}

// TestHandleUpdateGatesLibBinOnlyOnWindows is the cross-platform complement
// (skipped on Windows): a direct call to shouldSyncLibBin confirms the
// current host is gated OFF for lib/bin, matching the live handleUpdate path.
// This documents the live-host behavior the integration test above relies on.
func TestHandleUpdateGatesLibBinOnlyOnWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this test documents the non-Windows gate; run on linux/darwin")
	}
	if shouldSyncLibBin(runtime.GOOS) {
		t.Fatalf("shouldSyncLibBin(%q) = true, want false on non-Windows", runtime.GOOS)
	}
}

// context import is referenced via the manifest path's downstream callers in
// other agent tests; kept to avoid an unused-import churn if the file grows.
var _ = context.Background
