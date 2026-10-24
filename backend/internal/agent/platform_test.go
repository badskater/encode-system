package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestHeartbeatCarriesPlatform verifies the agent's heartbeat payload carries
// a platform field matching runtime.GOOS, so the controller can select the
// right script dialect per node.
func TestHeartbeatCarriesPlatform(t *testing.T) {
	want := "Bearer " + nodeTok()
	var got atomic.Value // string
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/agent/heartbeat" {
			if r.Header.Get("Authorization") != want {
				w.WriteHeader(401)
				return
			}
			body, _ := io.ReadAll(r.Body)
			got.Store(string(body))
			w.Write([]byte(`{"instruction":"none"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer controller.Close()

	a, _ := New(Config{ControllerURL: controller.URL, NodeName: "n", Token: nodeTok(), DataDir: t.TempDir(), HeartbeatEvery: 1}, "v", testLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := got.Load().(string); ok && v != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done

	body, _ := got.Load().(string)
	if body == "" {
		t.Fatal("no heartbeat captured")
	}
	var hb map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &hb); err != nil {
		t.Fatalf("unmarshal heartbeat: %v", err)
	}
	raw, ok := hb["platform"]
	if !ok {
		t.Fatalf("heartbeat missing platform field: %s", body)
	}
	var p string
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal platform: %v", err)
	}
	if p != runtime.GOOS {
		t.Fatalf("platform = %q, want %q (runtime.GOOS)", p, runtime.GOOS)
	}
}
