package scanner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestRunLoopTriggerFiresImmediately verifies the trigger channel causes a
// scan on the next select cycle, without waiting for the ticker interval.
func TestRunLoopTriggerFiresImmediately(t *testing.T) {
	root := t.TempDir()
	mkEp(t, root, "S", "Ep 01", "src.m2ts", "1080.vpy")
	past := time.Now().Add(-10 * time.Minute)
	os.Chtimes(filepath.Join(root, "S", "Ep 01", "src.m2ts"), past, past)

	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 1, Name: "default-1080"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	trigger := make(chan struct{}, 1)
	cfg := func(ctx context.Context) (Target, time.Duration, string) {
		return Target{Root: root}, time.Hour, "default-1080" // long interval so only trigger fires
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, log, st, cfg, trigger)
		close(done)
	}()

	// Fire the trigger; a job should be created well before the 1h ticker.
	select {
	case trigger <- struct{}{}:
	case <-time.After(time.Second):
		t.Fatal("could not send on trigger channel")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.created)
		st.mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trigger did not cause a scan: %d jobs created", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunLoopTriggerNilSafe verifies RunLoop works with a nil trigger channel
// (the no-trigger path — only the ticker fires). This guards the call site
// that passes nil when the feature is unused.
func TestRunLoopTriggerNilSafe(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := &fakeStore{existing: map[string]bool{}, flow: &model.Flow{ID: 1, Name: "default-1080"}}

	var mu sync.Mutex
	calls := 0
	interval := 30 * time.Millisecond
	cfg := func(ctx context.Context) (Target, time.Duration, string) {
		mu.Lock()
		calls++
		mu.Unlock()
		return Target{}, interval, "default-1080"
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunLoop(ctx, log, st, cfg, nil) // nil trigger
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 2 {
		t.Fatalf("nil trigger: loop did not cycle via ticker: %d calls", n)
	}
}
