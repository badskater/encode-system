package agent

import "testing"

// TestShouldSyncLibBin verifies the platform gate that decides whether the
// EncodeLib.ps1 + bin-folder sync steps run. They are Windows-only: the lib
// is PowerShell and the bin package holds Windows tooling extracted to
// C:\bin. On any other platform (linux, darwin) the steps are skipped — the
// agent-binary sync still runs (the server serves a platform-scoped
// payload). This is the core of the merge-blocker fix: a Linux node must
// never try to sync Windows-only artifacts.
func TestShouldSyncLibBin(t *testing.T) {
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
