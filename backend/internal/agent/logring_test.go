package agent

import (
	"fmt"
	"strings"
	"testing"
)

// TestLogRingKeepsBoundedLines asserts the ring evicts oldest lines and
// Tail() returns newest-last joined output.
func TestLogRingKeepsBoundedLines(t *testing.T) {
	r := NewLogRing()
	for i := 0; i < logRingLines+10; i++ {
		fmt.Fprintf(r, "line-%d\n", i)
	}
	tail := r.Tail()
	lines := strings.Split(tail, "\n")
	if len(lines) != logRingLines {
		t.Fatalf("want %d lines, got %d", logRingLines, len(lines))
	}
	if lines[len(lines)-1] != fmt.Sprintf("line-%d", logRingLines+9) {
		t.Fatalf("newest line must be last: %q", lines[len(lines)-1])
	}
	if lines[0] != "line-10" {
		t.Fatalf("oldest surviving line: want line-10, got %q", lines[0])
	}
}

// TestLogRingEmptyTail asserts an unwritten ring ships "" (heartbeat omits
// the field) rather than a bare newline.
func TestLogRingEmptyTail(t *testing.T) {
	if got := NewLogRing().Tail(); got != "" {
		t.Fatalf("want empty tail, got %q", got)
	}
}

// TestLogRingByteCap asserts a pathological fat line cannot exceed the
// wire cap, the newest lines always survive, and the front cut lands on a
// line boundary — the first entry of the tail is a COMPLETE line, never a
// mid-line fragment.
func TestLogRingByteCap(t *testing.T) {
	r := NewLogRing()
	fat := strings.Repeat("x", logRingBytes) // one line at the cap by itself
	fmt.Fprintf(r, "%s\nfirst-after-fat\nsecond\nrecent\n", fat)
	tail := r.Tail()
	if len(tail) > logRingBytes {
		t.Fatalf("tail exceeds cap: %d bytes", len(tail))
	}
	if !strings.HasSuffix(tail, "recent") {
		t.Fatalf("newest line must survive truncation: %q", tail[max(0, len(tail)-40):])
	}
	// Line-boundary contract, directly: the FIRST line of the tail must be
	// one of the complete lines we pushed. If the cut left a fragment of
	// the fat line, the first entry would be an all-x string that is
	// shorter than the fat line we pushed — reject that explicitly.
	first := tail[:strings.IndexByte(tail, '\n')]
	switch first {
	case "first-after-fat", "second", "recent":
		// a complete known line — correct boundary cut
	default:
		if strings.Trim(first, "x") == "" && first != fat {
			t.Fatalf("front cut left a mid-line fragment as first entry: %d x-chars", len(first))
		}
		t.Fatalf("unexpected first tail line: %q", first)
	}
	if !strings.Contains(tail, "first-after-fat") {
		t.Fatalf("intact post-fat lines missing from tail: %q", tail[:80])
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestLogRingPartialWritesAssemble verifies the io.Writer contract: a line
// split across multiple Writes lands in the ring as ONE entry, and an
// unterminated fragment surfaces in Tail() as the newest (pending) entry.
func TestLogRingPartialWritesAssemble(t *testing.T) {
	r := NewLogRing()
	fmt.Fprint(r, `{"level":"INFO","ms`)
	fmt.Fprint(r, `g":"split line"}`)
	fmt.Fprint(r, "\n")
	fmt.Fprint(r, "unterminated-tail")

	tail := r.Tail()
	lines := strings.Split(tail, "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 entries (1 assembled line + 1 pending), got %d: %q", len(lines), tail)
	}
	if lines[0] != `{"level":"INFO","msg":"split line"}` {
		t.Fatalf("split write must assemble into one entry: %q", lines[0])
	}
	if lines[1] != "unterminated-tail" {
		t.Fatalf("pending fragment must surface as newest entry: %q", lines[1])
	}
}

// TestLogRingPendingCap verifies an unterminated pending buffer cannot grow
// without bound (writer that never emits a newline).
func TestLogRingPendingCap(t *testing.T) {
	r := NewLogRing()
	fmt.Fprint(r, strings.Repeat("z", logRingBytes*3))
	tail := r.Tail()
	if len(tail) > logRingBytes {
		t.Fatalf("pending overflowed the wire cap: %d bytes", len(tail))
	}
}
