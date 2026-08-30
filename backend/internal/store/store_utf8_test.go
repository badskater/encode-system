package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCapFullLogRuneBoundary reproduces the UTF-8 rune-splitting bug: when the
// byte cut lands inside a multibyte sequence, the truncated body started with
// continuation bytes → invalid UTF-8. Encode logs carry CJK series names (the
// reviewer reproduced this with repeating 中), so this is realistic input. The
// fix aligns the cut to a rune boundary before searching for the newline.
//
// 中 is the 3-byte sequence 0xE4 0xB8 0xAD. We build valid-UTF-8 inputs (whole
// runes only) of several sizes just over 1 MiB so the raw byte cut lands at
// offset 1 or 2 inside a 中 rune (cut ≡ 1 or 2 mod 3).
func TestCapFullLogRuneBoundary(t *testing.T) {
	runeBytes := []byte("中") // E4 B8 AD
	if len(runeBytes) != 3 {
		t.Fatalf("expected 中 to be 3 bytes, got %d", len(runeBytes))
	}
	for _, extraRunes := range []int{1, 2, 3, 4} {
		t.Run(("extraRunes=" + itoaS(extraRunes)), func(t *testing.T) {
			// Whole 中 runes only — input is always valid UTF-8. Size is a
			// multiple of 3, > 1 MiB, so cut = size-1MiB lands at a position
			// whose mod-3 varies, exercising in-rune cuts.
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
			if len(input) != size {
				t.Fatalf("input size %d != expected %d", len(input), size)
			}

			out := capFullLog(input)

			// Must start with the truncation marker.
			if !strings.HasPrefix(out, truncationMarker) {
				t.Fatalf("output must start with marker; got prefix %q", safePrefix(out, 40))
			}
			// MUST be valid UTF-8 — this is the core bug assertion.
			if !utf8.ValidString(out) {
				body := strings.TrimPrefix(out, truncationMarker)
				t.Fatalf("output is invalid UTF-8 (body first bytes %s)", safeBytePrefix(body, 12))
			}
			// Total size stays within cap + marker.
			if len(out) > maxFullLogBytes+len(truncationMarker) {
				t.Fatalf("output %d exceeds cap+marker %d", len(out), maxFullLogBytes+len(truncationMarker))
			}
			// No data loss beyond alignment: body must be a suffix of the input.
			body := strings.TrimPrefix(out, truncationMarker)
			if !strings.HasSuffix(input, body) {
				t.Fatalf("body is not a suffix of input (data loss); body tail %q vs input tail %q",
					safeSuffix(body, 20), safeSuffix(input, 20))
			}
		})
	}
}

// TestCapFullLogNoNewline verifies that an input >1MiB with ZERO newlines does
// not panic, is bounded, carries the marker, and stays valid UTF-8. Before the
// fix the raw byte cut split a rune → invalid UTF-8 persisted to the row.
func TestCapFullLogNoNewline(t *testing.T) {
	// A single giant run of 中 (3 bytes each), no newlines, just over 1 MiB.
	runeCount := maxFullLogBytes/3 + 100 // > 1 MiB of bytes
	var sb strings.Builder
	sb.Grow(runeCount * 3)
	for i := 0; i < runeCount; i++ {
		sb.WriteString("中")
	}
	input := sb.String()
	if !utf8.ValidString(input) {
		t.Fatalf("test setup: input must be valid UTF-8")
	}

	out := capFullLog(input)

	if !strings.HasPrefix(out, truncationMarker) {
		t.Fatalf("output must start with marker; got prefix %q", safePrefix(out, 40))
	}
	if len(out) > maxFullLogBytes+len(truncationMarker) {
		t.Fatalf("output %d exceeds cap+marker %d", len(out), maxFullLogBytes+len(truncationMarker))
	}
	if !utf8.ValidString(out) {
		t.Fatalf("output is invalid UTF-8")
	}
}

// TestCapFullLogExact verifies the exact boundary: an input of exactly 1 MiB is
// returned verbatim (no marker), and 1MiB+1 byte is capped (marker present).
func TestCapFullLogExact(t *testing.T) {
	// Exactly at cap: verbatim, no marker.
	exact := strings.Repeat("x", maxFullLogBytes)
	out := capFullLog(exact)
	if out != exact {
		t.Fatalf("input at cap must be verbatim; got %d bytes (marker? %v)", len(out), strings.HasPrefix(out, truncationMarker))
	}

	// One byte over: must be capped.
	over := strings.Repeat("x", maxFullLogBytes+1)
	out2 := capFullLog(over)
	if !strings.HasPrefix(out2, truncationMarker) {
		t.Fatalf("input over cap must start with marker; got prefix %q", safePrefix(out2, 40))
	}
	if len(out2) > maxFullLogBytes+len(truncationMarker) {
		t.Fatalf("capped output %d exceeds cap+marker %d", len(out2), maxFullLogBytes+len(truncationMarker))
	}
}

// --- helpers ---

func itoaS(n int) string {
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

func safePrefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func safeSuffix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[len(s)-n:]
}

func safeBytePrefix(b string, n int) string {
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
