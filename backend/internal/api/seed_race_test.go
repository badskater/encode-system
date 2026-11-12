package api

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/store"
)

// TestSeedDefaultFlowConcurrentBoot simulates HA replicas booting against
// the SAME fresh database at once (Q6). seedDefaultFlow is check-then-act:
// two replicas can both pass the FlowByName check before either inserts,
// and the loser's CreateFlow hits the flows.name UNIQUE constraint. The
// loser must treat that as "already seeded" (re-check, continue silently)
// instead of failing startup. N goroutines race on a fresh store; the
// boot must be error-free and leave EXACTLY one flow per factory name.
func TestSeedDefaultFlowConcurrentBoot(t *testing.T) {
	const racers = 16
	for iter := 0; iter < 3; iter++ {
		dir := t.TempDir()
		st, err := store.Open(dir + "/test.db")
		if err != nil {
			t.Fatal(err)
		}
		// Construct the Server directly (New() would seed once up front,
		// hiding the race): only Store/Log/Cfg are used by seedDefaultFlow.
		s := &Server{
			Store: st,
			Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Cfg:   Config{DefaultFlowName: "default-1080"},
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, racers)
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start // release all racers together
				if err := s.seedDefaultFlow(); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("iter %d: concurrent seed boot failed: %v", iter, err)
		}
		ctx := context.Background()
		flows, err := st.ListFlows(ctx)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, f := range flows {
			counts[f.Name]++
		}
		for _, name := range []string{"default-1080", "default-4k", "default-4k-cpu"} {
			if counts[name] != 1 {
				t.Errorf("iter %d: flow %q seeded %d times, want exactly 1", iter, name, counts[name])
			}
		}
		if len(flows) != 3 {
			t.Errorf("iter %d: %d flows after concurrent seed, want 3", iter, len(flows))
		}
		st.Close()
	}
}
