package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCaptureRunLogRuneBoundary reproduces the UTF-8 rune-splitting bug on the
// agent side: captureRunLog computes the cut as a raw byte offset, so if it
// lands inside a multibyte sequence the body starts with continuation bytes →
// invalid UTF-8 served in the job completion report. The fix aligns the cut
// to a rune boundary. 中 = 0xE4 0xB8 0xAD (3 bytes). Inputs are whole-rune
// (valid UTF-8) so the START alignment is what's under test.
func TestCaptureRunLogRuneBoundary(t *testing.T) {
	runeBytes := []byte("中")
	if len(runeBytes) != 3 {
		t.Fatalf("expected 中 to be 3 bytes, got %d", len(runeBytes))
	}
	for _, extraRunes := range []int{1, 2, 3, 4} {
		t.Run(("extraRunes=" + itoaA(extraRunes)), func(t *testing.T) {
			runeCount := maxFullLogBytes/3 + extraRunes
			size := runeCount * 3
			var sb strings.Builder
			sb.Grow(size)
			for i := 0; i < runeCount; i++ {
				sb.Write(runeBytes)
			}
			input := sb.String()
			if !utf8.ValidString(input) {
				t.Fatalf("test setup: input must be valid UTF-8")
			}

			dir := t.TempDir()
			path := filepath.Join(dir, "run.log")
			if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := captureRunLog(path)
			if err != nil {
				t.Fatalf("captureRunLog error: %v", err)
			}

			// Must start with the truncation marker.
			if !strings.HasPrefix(got, truncationMarker) {
				t.Fatalf("output must start with marker; got prefix %q", safePrefixA(got, 40))
			}
			// MUST be valid UTF-8 — the core bug assertion.
			if !utf8.ValidString(got) {
				body := strings.TrimPrefix(got, truncationMarker)
				t.Fatalf("output is invalid UTF-8 (body first bytes %s)", safeBytePrefixA(body, 12))
			}
			// Total size within cap + marker.
			if len(got) > maxFullLogBytes+len(truncationMarker) {
				t.Fatalf("output %d exceeds cap+marker %d", len(got), maxFullLogBytes+len(truncationMarker))
			}
			// No data loss beyond alignment: body must be a suffix of input.
			body := strings.TrimPrefix(got, truncationMarker)
			if !strings.HasSuffix(input, body) {
				t.Fatalf("body is not a suffix of input (data loss)")
			}
		})
	}
}

// --- helpers (underscore-suffixed to avoid colliding with the existing test
// file's helpers in the same package) ---

func itoaA(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func safePrefixA(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func safeBytePrefixA(b string, n int) string {
	if len(b) < n {
		n = len(b)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString("0x")
		sb.WriteByte("0123456789abcdef"[b[i]>>4])
		sb.WriteByte("0123456789abcdef"[b[i]&0xf])
		sb.WriteByte(' ')
	}
	return sb.String()
}
