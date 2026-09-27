package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// Metrics collection budget: the total time collectMetrics may spend across
// all collectors. Heartbeats must never stall — a slow WMI query or a hung
// nvidia-smi must not block the heartbeat interval. 4 s is well under the
// 15 s default heartbeat cadence and leaves headroom for the POST itself.
const metricsTimeout = 4 * time.Second

// tailReadSize is the maximum number of bytes read from run.log for FPS
// parsing. A multi-gigabyte encode log must never be fully scanned; the
// last 64 KiB always contains the most recent progress line.
const tailReadSize = 64 * 1024

// runLogMu guards runLogPathVal for concurrent access between the heartbeat
// goroutine (reader) and executeJob (writer). We use a dedicated mutex
// rather than a.mu to avoid coupling FPS reads to job-state mutations.
type runLogGuard struct {
	mu   sync.Mutex
	path string
}

func (g *runLogGuard) set(p string) {
	g.mu.Lock()
	g.path = p
	g.mu.Unlock()
}

func (g *runLogGuard) get() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.path
}

// psFunc is the injectable exec seam for PowerShell one-liners. Tests
// inject a fake to verify parsing without shelling out; production uses
// the real exec.Command wrapper below.
type psFunc func(ctx context.Context, script string) (string, error)

// gpuFunc is the injectable exec seam for nvidia-smi probes. Returns
// (util, temp, memUsedMB) or (-1, -1, -1) when no GPU is present.
type gpuFunc func(ctx context.Context) (int, int, int)

// runPS exec captures a PowerShell one-liner's stdout. Production
// implementation; tests replace a.runPS to inject fake output.
func (a *Agent) runPSDefault(ctx context.Context, script string) (string, error) {
	ps := a.findPowerShell()
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gpuProbeDefault looks for nvidia-smi and, if present, queries utilization,
// temperature, and memory used. Returns -1 for all GPU fields if the binary
// is absent or the output is unparseable. Silent on failure (debug log only).
func (a *Agent) gpuProbeDefault(ctx context.Context) (int, int, int) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return -1, -1, -1 // no GPU binary on this host
	}
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=utilization.gpu,temperature.gpu,memory.used",
		"--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		a.Log.Debug("nvidia-smi exec failed", "err", err)
		return -1, -1, -1
	}
	return parseNvidiaSmiCSV(strings.TrimSpace(string(out)))
}

// parseFlexibleFloat parses a float from PowerShell output, tolerating both
// '.' and ',' decimal separators (e.g. a de-DE localized Windows host emits
// "42,5"). It trims whitespace and a leading UTF-8 BOM. Returns an error
// for garbage or inputs with both separators.
func parseFlexibleFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	// Strip a leading UTF-8 BOM (PowerShell console capture may include it).
	s = strings.TrimPrefix(s, "\ufeff")
	if s == "" {
		return 0, fmt.Errorf("empty float")
	}
	// If the value uses a comma as decimal separator and there is no dot,
	// normalize to a dot so strconv.ParseFloat understands it. If both
	// separators are present, the string is ambiguous → reject.
	if strings.ContainsRune(s, ',') {
		if strings.ContainsRune(s, '.') {
			return 0, fmt.Errorf("ambiguous decimal separators: %q", s)
		}
		s = strings.ReplaceAll(s, ",", ".")
	}
	return strconv.ParseFloat(s, 64)
}

// --- pure, unit-testable parsers ---

// fpsPatterns matches encode progress FPS indicators. We keep two patterns:
// x265's "N.NN fps" (with the comma/ETA suffix) and ffmpeg's "fps= N.N".
// The last match in the sample wins because encoders report continuously.
var (
	fpsX265Re   = regexp.MustCompile(`(\d+(?:\.\d+)?)\s+fps`)
	fpsFfmpegRe = regexp.MustCompile(`fps=\s*(\d+(?:\.\d+)?)`)
)

// parseEncodeFps scans a log sample for the last FPS indicator (x265 or
// ffmpeg) and returns the value. Returns 0 when no match is found (no
// encode running, or the encoder hasn't reported FPS yet). The function
// is pure and platform-independent so it is unit-tested on Linux.
func parseEncodeFps(sample string) float64 {
	var last float64
	found := false
	for _, line := range strings.Split(sample, "\n") {
		// ffmpeg: "fps= 23" or "fps= 29.97"
		if m := fpsFfmpegRe.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				last = v
				found = true
			}
			continue
		}
		// x265: "12.34 fps" — but guard against matching the ffmpeg
		// pattern's "fps=" remnant; the x265 pattern matches a bare
		// number followed by " fps" (with a space).
		if m := fpsX265Re.FindStringSubmatch(line); m != nil {
			if v, err := strconv.ParseFloat(m[1], 64); err == nil {
				last = v
				found = true
			}
		}
	}
	if !found {
		return 0
	}
	return last
}

// parseNvidiaSmiCSV parses a single-row nvidia-smi CSV output
// ("util, temp, memMiB"). Returns (-1, -1, -1) on any parse failure.
// If multiple rows are present, the first is used (typical single-GPU node).
func parseNvidiaSmiCSV(s string) (util, temp, mem int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1, -1, -1
	}
	row := s
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		row = s[:idx] // first row only
	}
	row = strings.TrimSpace(row)
	parts := strings.Split(row, ",")
	if len(parts) < 3 {
		return -1, -1, -1
	}
	var err error
	util, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return -1, -1, -1
	}
	temp, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return -1, -1, -1
	}
	mem, err = strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil {
		return -1, -1, -1
	}
	return util, temp, mem
}

// driveLetterFromPath extracts the Windows drive letter from a path like
// "C:\foo" → "C". Returns "" for Unix paths and UNC paths. Pure Go
// (manual scan, NOT filepath.VolumeName which is OS-dependent and returns
// "" for Windows paths on Linux) so it works identically on all platforms
// — critical for unit-testing on Linux.
func driveLetterFromPath(p string) string {
	// A Windows drive letter is a single alpha char followed by ':'.
	// UNC paths (\\server\share) have no drive letter.
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return strings.ToUpper(string(c))
		}
	}
	return ""
}

// readTailBytes reads at most the last `max` bytes of a file, seeking from
// the end. If the file is smaller than `max`, the entire content is
// returned. This bounds FPS parsing: a multi-GB run.log never gets fully
// read. On read error (file missing, permission denied) returns the error
// so the caller can degrade to 0 FPS.
//
// When the file is larger than `max`, the seek lands mid-line; the first
// partial line fragment (everything up to and including the first '\n') is
// discarded so downstream parsers see only complete lines. When the file
// fits within `max` (offset 0), everything is kept as-is.
func readTailBytes(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	if size <= int64(max) {
		return os.ReadFile(path) // small file: read whole
	}
	// Seek to end - max, then read the tail.
	offset := size - int64(max)
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, max)
	// FIX: use io.ReadFull instead of a single f.Read. A single Read may
	// return fewer bytes than requested (allowed by the io.Reader contract),
	// silently truncating the tail. io.ReadFull reads exactly len(buf) bytes
	// unless the file ends early (→ io.ErrUnexpectedEOF, which we tolerate
	// since the partial read is still valid data).
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && n == 0 {
		return nil, err
	}
	result := buf[:n]
	// FIX: when we seeked into the middle of the file (offset > 0), the
	// buffer starts mid-line. Discard everything up to and including the
	// first '\n' so FPS parsing sees only complete log lines. Best-effort:
	// if there is no newline at all, keep the data rather than returning empty.
	if idx := bytes.IndexByte(result, '\n'); idx >= 0 {
		result = result[idx+1:]
	}
	return result, nil
}

// --- Agent struct additions and accessors ---

// runLogGuard holds the current job's run.log path, guarded for concurrent
// heartbeat reads while executeJob sets/clears it.
// setRunLogPath is the legacy single-path setter, kept for tests that
// exercise the guard directly. The live path now comes from the job
// registry (per-job run.log slots); see currentRunLogPath.
func (a *Agent) setRunLogPath(p string) { a.rlGuard.set(p) }

// currentRunLogPath resolves the run.log the FPS parser should read: the
// newest active job's log when jobs are running (registry), else the
// legacy guard value (set/cleared by direct callers and tests).
func (a *Agent) currentRunLogPath() string {
	if p := a.registry().newestRunLog(); p != "" {
		return p
	}
	return a.rlGuard.get()
}

// --- collection orchestrator ---

// collectMetrics runs each collector with a bounded timeout budget and
// returns a populated *model.NodeMetrics. ANY failure in a collector
// results in that field being omitted/zero/-1, but collectMetrics itself
// NEVER fails and NEVER returns nil — the Heartbeat.Metrics pointer is
// set unconditionally. The total budget is capped at metricsTimeout so a
// hung WMI query cannot stall the heartbeat interval.
func (a *Agent) collectMetrics(ctx context.Context) *model.NodeMetrics {
	m := &model.NodeMetrics{
		GPUUtil: -1, GPUTemp: -1, GPUMemUsedMB: -1, // no GPU until proven
	}

	// Bound the whole collection so one slow collector does not cascade.
	cctx, cancel := context.WithTimeout(ctx, metricsTimeout)
	defer cancel()

	// resolve the exec seams: use injected fakes in tests, defaults in prod.
	// A nil seam means "not on this platform or not configured"; the
	// collector is skipped (field stays at its zero value).
	psRun := a.runPS
	gpuRun := a.gpuProbe
	if gpuRun == nil {
		gpuRun = a.gpuProbeDefault
	}

	// CPU %. The runPS seam is nil on non-Windows (tests inject fakes to
	// exercise the parsing path on Linux without shelling out).
	if psRun != nil {
		if out, err := psRun(cctx, cpuScript); err == nil {
			if v, perr := parseFlexibleFloat(out); perr == nil {
				m.CPUPct = v
			}
		}
	}

	// RAM.
	if psRun != nil {
		if out, err := psRun(cctx, ramScript); err == nil {
			parseRAM(out, m)
		}
	}

	// Disk free GB.
	if psRun != nil {
		if out, err := psRun(cctx, a.diskScript()); err == nil {
			if v, perr := parseFlexibleFloat(out); perr == nil {
				m.DiskFreeGB = int64(v)
			}
		}
	}

	// GPU. Guarded: most test VMs have no nvidia-smi → silent -1 fields.
	util, temp, mem := gpuRun(cctx)
	m.GPUUtil = util
	m.GPUTemp = temp
	m.GPUMemUsedMB = mem

	// Encode FPS. Parse the tail of the current job's run.log (if any).
	if p := a.currentRunLogPath(); p != "" {
		if data, err := readTailBytes(p, tailReadSize); err == nil {
			m.EncodeFPS = parseEncodeFps(string(data))
		}
	}

	return m
}

// --- PowerShell one-liners ---

const cpuScript = "(Get-CimInstance Win32_Processor | Measure-Object LoadPercentage -Average).Average"

const ramScript = "(Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize; " +
	"(Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory"

// diskScript builds the PowerShell one-liner to query free space (in GB)
// on the drive holding the agent's release/scripts dir. Falls back to the
// drive of DataDir if neither is configured. Uses [IO.DriveInfo] which is
// available in all PowerShell versions.
func (a *Agent) diskScript() string {
	drive := ""
	// Prefer the scripts dir, then the release dir, then the data dir.
	for _, candidate := range []string{a.Cfg.BinDir, a.Cfg.DataDir} {
		if candidate == "" {
			continue
		}
		if d := driveLetterFromPath(candidate); d != "" {
			drive = d
			break
		}
	}
	if drive == "" {
		drive = "C"
	}
	return fmt.Sprintf(
		"[math]::Round((Get-CimInstance Win32_LogicalDisk -Filter \"DeviceID='%s:'\").FreeSpace / 1GB, 0)",
		drive,
	)
}

// parseRAM parses the two-line WMI output (total KB, free KB) and populates
// the metrics struct. Total is the first line; free is the second. Used KB
// = total - free. All values are converted to MB. Handles a leading UTF-8
// BOM and CRLF line endings, both common in PowerShell console capture.
func parseRAM(s string, m *model.NodeMetrics) {
	// Strip a leading UTF-8 BOM (PowerShell console capture may include it).
	s = strings.TrimPrefix(s, "\ufeff")
	// Normalize CRLF to LF so Split on '\n' yields clean values.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 2 {
		return
	}
	totalKB, err1 := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	freeKB, err2 := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if err1 != nil || err2 != nil {
		return
	}
	m.MemTotalMB = totalKB / 1024
	usedKB := totalKB - freeKB
	if usedKB < 0 {
		usedKB = 0
	}
	m.MemUsedMB = usedKB / 1024
}
