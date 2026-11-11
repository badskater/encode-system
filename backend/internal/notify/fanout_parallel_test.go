package notify

// Regression for spec gap G5: the fan-out delivered channels SEQUENTIALLY,
// so N slow transports stacked their timeouts inside request handlers
// (worst case ~50s: 4 channels × 10s HTTP bound + SMTP). Delivery must be
// parallel (total ≈ max, not sum) while keeping per-channel panic
// containment and the existing per-channel bounds.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// slowChannel sleeps for delay on every event, then records it.
type slowChannel struct {
	name   string
	delay  time.Duration
	mu     sync.Mutex
	jobs   int
	alerts int
}

func (c *slowChannel) Name() string { return c.name }

func (c *slowChannel) JobFinished(_ context.Context, _ *model.Job, _ string) {
	time.Sleep(c.delay)
	c.mu.Lock()
	c.jobs++
	c.mu.Unlock()
}

func (c *slowChannel) Alert(_ context.Context, _ string) {
	time.Sleep(c.delay)
	c.mu.Lock()
	c.alerts++
	c.mu.Unlock()
}

func (c *slowChannel) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jobs, c.alerts
}

// TestFanoutParallelDelivery: three 300ms channels must complete in ≈300ms
// (parallel), not ≈900ms (sequential). Both event kinds are checked, and
// every channel must still receive every event.
func TestFanoutParallelDelivery(t *testing.T) {
	const delay = 300 * time.Millisecond
	chs := []Channel{
		&slowChannel{name: "a", delay: delay},
		&slowChannel{name: "b", delay: delay},
		&slowChannel{name: "c", delay: delay},
	}
	f := NewFanout(chs, testLog())

	start := time.Now()
	f.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobDone}, "n")
	elapsed := time.Since(start)
	if elapsed >= 2*delay {
		t.Fatalf("JobFinished took %v with 3×%v channels — sequential, not parallel", elapsed, delay)
	}
	if elapsed < delay {
		t.Fatalf("JobFinished took %v — channels did not actually run", elapsed)
	}

	start = time.Now()
	f.Alert(context.Background(), "x")
	elapsed = time.Since(start)
	if elapsed >= 2*delay {
		t.Fatalf("Alert took %v with 3×%v channels — sequential, not parallel", elapsed, delay)
	}

	for _, ch := range chs {
		sc := ch.(*slowChannel)
		if jobs, alerts := sc.counts(); jobs != 1 || alerts != 1 {
			t.Errorf("channel %s: jobs=%d alerts=%d, want 1/1", sc.name, jobs, alerts)
		}
	}
}

// TestFanoutParallelPanicContainment: a panicking channel among slow ones
// must not stop the others (isolation survives parallelization).
func TestFanoutParallelPanicContainment(t *testing.T) {
	a := &slowChannel{name: "a", delay: 50 * time.Millisecond}
	bad := &recordingChannel{name: "bad", panic: true}
	b := &slowChannel{name: "b", delay: 50 * time.Millisecond}
	f := NewFanout([]Channel{a, bad, b}, testLog())

	f.JobFinished(context.Background(), &model.Job{ID: 1, Status: model.JobFailed}, "n")
	f.Alert(context.Background(), "x")

	if jobs, alerts := a.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel a: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
	if jobs, alerts := b.counts(); jobs != 1 || alerts != 1 {
		t.Errorf("channel b: jobs=%d alerts=%d, want 1/1", jobs, alerts)
	}
}
