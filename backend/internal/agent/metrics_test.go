package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// --- drive letter helper ---

func TestDriveLetterFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{`C:\foo\bar`, "C"},
		{`C:/foo/bar`, "C"},
		{`D:\`, "D"},
		{`D:`, "D"},
		{`/unix/path`, ""},
		{`relative/path`, ""},
		{``, ""},
		{`\\server\share`, ""}, // UNC path has no drive letter
	}
	for _, c := range cases {
		got := driveLetterFromPath(c.path)
		if got != c.want {
			t.Errorf("driveLetterFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// --- FPS parser ---

func TestParseEncodeFps(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  float64
	}{
		{
			"x265 fps line",
			"x265 [info]: frame=  120 [...] 12.34 fps, ETA 1:23:45",
			12.34,
		},
		{
			"ffmpeg fps= line",
			"frame=  500 fps= 23 q=-0.0 size=    1280kB time=00:00:20.00",
			23.0,
		},
		{
			"multiple lines last wins",
			"x265 [info]: frame=  100 [...] 5.00 fps, ETA 2:00:00\n" +
				"x265 [info]: frame=  200 [...] 10.50 fps, ETA 1:00:00",
			10.50,
		},
		{"no match", "some random log line with no fps", 0},
		{"empty string", "", 0},
		{"garbage", "@#$%^&*()", 0},
		{
			"ffmpeg with decimal fps",
			"frame= 1024 fps= 29.97 q=28.0 size=    2560kB",
			29.97,
		},
		{
			"x265 and ffmpeg mixed, ffmpeg is last",
			"x265 [info]: frame=  120 [...] 15.00 fps, ETA 1:23:45\n" +
				"frame=  500 fps= 42 q=-0.0 size=...",
			42.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseEncodeFps(c.input)
			if got != c.want {
				t.Errorf("parseEncodeFps() = %v, want %v", got, c.want)
			}
		})
	}
}

// --- nvidia-smi CSV parser ---

func TestParseNvidiaSmiCSV(t *testing.T) {
	cases := []struct {
		name  string
		input string
		util  int
		temp  int
		mem   int
	}{
		{"normal row", "30, 65, 1024", 30, 65, 1024},
		{"spaces around", " 12 ,  55 ,  512 ", 12, 55, 512},
		{"malformed", "abc, def, ghi", -1, -1, -1},
		{"empty", "", -1, -1, -1},
		{"multiple rows first wins", "30, 65, 1024\n50, 70, 2048", 30, 65, 1024},
		{"two columns missing one", "30, 65", -1, -1, -1},
		{"extra whitespace newline", "\n  10, 40, 256  \n", 10, 40, 256},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			util, temp, mem := parseNvidiaSmiCSV(c.input)
			if util != c.util || temp != c.temp || mem != c.mem {
				t.Errorf("parseNvidiaSmiCSV(%q) = (%d, %d, %d), want (%d, %d, %d)",
					c.input, util, temp, mem, c.util, c.temp, c.mem)
			}
		})
	}
}

// --- bounded tail reader ---

func TestReadTailBytes(t *testing.T) {
	// File smaller than cap → whole content returned.
	t.Run("small file returns whole content", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "small.log")
		content := strings.Repeat("line\n", 10)
		os.WriteFile(p, []byte(content), 0o644)
		got, err := readTailBytes(p, 64*1024)
		if err != nil {
			t.Fatalf("readTailBytes err: %v", err)
		}
		if string(got) != content {
			t.Errorf("small file: got %q, want %q", string(got), content)
		}
	})

	// File larger than cap → last cap bytes returned.
	t.Run("large file returns last chunk", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "big.log")
		// 100 KiB of content; cap at 4 KiB.
		chunk := strings.Repeat("A", 1024)
		full := strings.Repeat(chunk, 100) // 100 KiB
		os.WriteFile(p, []byte(full), 0o644)
		got, err := readTailBytes(p, 4096)
		if err != nil {
			t.Fatalf("readTailBytes err: %v", err)
		}
		if len(got) != 4096 {
			t.Fatalf("large file: got %d bytes, want 4096", len(got))
		}
		// Must be the last 4096 bytes of the file.
		want := full[len(full)-4096:]
		if string(got) != want {
			t.Error("large file: tail content mismatch")
		}
	})

	// Non-existent file → error (caller handles gracefully).
	t.Run("missing file returns error", func(t *testing.T) {
		_, err := readTailBytes(filepath.Join(t.TempDir(), "nope.log"), 4096)
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}

// --- collectMetrics with injected exec ---

// TestCollectMetricsNoGPU verifies that when the runPS seam returns valid
// CPU/RAM data and nvidia-smi is absent, the GPU fields are -1 and the
// other fields are populated. This is the common case on CPU test VMs.
func TestCollectMetricsNoGPU(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())

	// Inject a fake runPS that returns Windows-like WMI output.
	a.runPS = func(ctx context.Context, script string) (string, error) {
		switch {
		case strings.Contains(script, "Win32_Processor"):
			return "42", nil // CPU load %
		case strings.Contains(script, "Win32_OperatingSystem"):
			return "8388608\n4194304", nil // total KB, free KB → 4 GB used, 8 GB total
		case strings.Contains(script, "Win32_LogicalDisk"):
			return "100", nil // 100 GB free
		default:
			return "", nil
		}
	}
	// No nvidia-smi on the path (test env).
	a.gpuProbe = func(ctx context.Context) (int, int, int) {
		return -1, -1, -1
	}

	m := a.collectMetrics(context.Background())
	if m == nil {
		t.Fatal("collectMetrics must never return nil")
	}
	if m.CPUPct != 42 {
		t.Errorf("CPUPct = %v, want 42", m.CPUPct)
	}
	if m.MemTotalMB != 8192 {
		t.Errorf("MemTotalMB = %d, want 8192", m.MemTotalMB)
	}
	if m.MemUsedMB != 4096 {
		t.Errorf("MemUsedMB = %d, want 4096", m.MemUsedMB)
	}
	if m.DiskFreeGB != 100 {
		t.Errorf("DiskFreeGB = %d, want 100", m.DiskFreeGB)
	}
	if m.GPUUtil != -1 || m.GPUTemp != -1 || m.GPUMemUsedMB != -1 {
		t.Errorf("GPU fields = (%d, %d, %d), want (-1, -1, -1)",
			m.GPUUtil, m.GPUTemp, m.GPUMemUsedMB)
	}
	if m.EncodeFPS != 0 {
		t.Errorf("EncodeFPS = %v, want 0 (no running job)", m.EncodeFPS)
	}
}

// TestCollectMetricsGPUFound verifies that when the GPU probe returns data,
// the GPU fields are populated in the metrics struct.
func TestCollectMetricsGPUFound(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())
	a.runPS = func(ctx context.Context, script string) (string, error) {
		return "", nil // no PS output → zero CPU/RAM/disk
	}
	a.gpuProbe = func(ctx context.Context) (int, int, int) {
		return 55, 72, 2048
	}

	m := a.collectMetrics(context.Background())
	if m.GPUUtil != 55 || m.GPUTemp != 72 || m.GPUMemUsedMB != 2048 {
		t.Errorf("GPU fields = (%d, %d, %d), want (55, 72, 2048)",
			m.GPUUtil, m.GPUTemp, m.GPUMemUsedMB)
	}
}

// TestCollectMetricsPSFailure verifies that a PS exec failure does not crash
// collectMetrics — the fields stay at zero and the struct is still returned.
func TestCollectMetricsPSFailure(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())
	a.runPS = func(ctx context.Context, script string) (string, error) {
		return "", os.ErrNotExist
	}
	a.gpuProbe = func(ctx context.Context) (int, int, int) {
		return -1, -1, -1
	}

	m := a.collectMetrics(context.Background())
	if m == nil {
		t.Fatal("must return non-nil even on total failure")
	}
	// Zero values are meaningful: 0 CPU, 0 RAM, 0 disk, -1 GPU.
	if m.CPUPct != 0 || m.MemUsedMB != 0 || m.DiskFreeGB != 0 {
		t.Errorf("expected zero values on PS failure, got %+v", m)
	}
}

// TestCollectMetricsEncodeFPS verifies that when a run.log path is set
// (a job is running), the FPS is parsed from the log tail and included.
func TestCollectMetricsEncodeFPS(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())
	a.runPS = func(ctx context.Context, script string) (string, error) {
		return "", nil
	}
	a.gpuProbe = func(ctx context.Context) (int, int, int) {
		return -1, -1, -1
	}

	// Write a fake run.log with an x265 fps line.
	runLog := filepath.Join(dir, "run.log")
	os.WriteFile(runLog, []byte(
		"x265 [info]: frame=  120 [...] 12.34 fps, ETA 1:23:45\n"+
			"some intermediate line\n"+
			"x265 [info]: frame=  200 [...] 15.50 fps, ETA 1:00:00\n",
	), 0o644)

	a.setRunLogPath(runLog)
	defer a.setRunLogPath("")

	m := a.collectMetrics(context.Background())
	if m.EncodeFPS != 15.50 {
		t.Errorf("EncodeFPS = %v, want 15.50", m.EncodeFPS)
	}
}

// TestHeartbeatIncludesMetrics verifies that the heartbeat payload carries
// the Metrics pointer when the agent constructs it.
func TestHeartbeatIncludesMetrics(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())
	a.runPS = func(ctx context.Context, script string) (string, error) {
		return "42", nil // CPU
	}
	a.gpuProbe = func(ctx context.Context) (int, int, int) {
		return -1, -1, -1
	}

	m := a.collectMetrics(context.Background())
	// Simulate the heartbeat construction code path.
	hb := model.Heartbeat{
		Node:         a.Cfg.NodeName,
		Metrics:      m,
		AgentVersion: a.Version,
	}
	if hb.Metrics == nil {
		t.Fatal("heartbeat must carry a non-nil Metrics pointer")
	}
	if hb.Metrics.CPUPct != 42 {
		t.Errorf("heartbeat metrics CPUPct = %v, want 42", hb.Metrics.CPUPct)
	}
}

// TestRunLogPathConcurrency verifies that setRunLogPath / currentRunLogPath
// are safe under concurrent access (heartbeat goroutine reads while
// executeJob writes). This is a smoke test — a data race would be caught
// by `go test -race`.
func TestRunLogPathConcurrency(t *testing.T) {
	dir := t.TempDir()
	a, _ := New(Config{
		ControllerURL: "http://x", NodeName: "n", Token: nodeTok(), DataDir: dir,
	}, "v", testLog())

	done := make(chan struct{})
	// Writer goroutine simulates executeJob setting/clearing the path.
	go func() {
		for i := 0; i < 100; i++ {
			a.setRunLogPath(filepath.Join(dir, "run.log"))
			a.setRunLogPath("")
		}
		close(done)
	}()
	// Reader goroutine simulates the heartbeat reading the path.
	for i := 0; i < 100; i++ {
		_ = a.currentRunLogPath()
	}
	<-done
}
