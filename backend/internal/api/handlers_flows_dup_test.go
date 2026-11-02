package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestFlowDuplicate copies a flow + its steps verbatim under a default name,
// with is_default=false. The original flow is untouched.
func TestFlowDuplicate(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	// Create a source flow with steps and a retry policy.
	src, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name: "dup-source",
		Steps: []model.Step{
			{Type: model.StepEncode, Params: map[string]string{"crf": "20"}},
			{Type: model.StepMux},
		},
		MaxRetries:          3,
		RetryBackoffMinutes: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/flows/"+itoa(src.ID)+"/duplicate", adminTok, map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var dup model.Flow
	if err := json.Unmarshal(body, &dup); err != nil {
		t.Fatalf("bad response %s: %v", body, err)
	}

	// Default name is "<orig> (copy)".
	if dup.Name != "dup-source (copy)" {
		t.Errorf("name = %q, want %q", dup.Name, "dup-source (copy)")
	}
	// Copies are never default.
	if dup.IsDefault {
		t.Errorf("duplicate is_default = true, want false")
	}
	// Steps copied verbatim.
	if len(dup.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(dup.Steps))
	}
	if dup.Steps[0].Type != model.StepEncode || dup.Steps[0].Params["crf"] != "20" {
		t.Errorf("step 0 mismatch: %+v", dup.Steps[0])
	}
	if dup.Steps[1].Type != model.StepMux {
		t.Errorf("step 1 mismatch: %+v", dup.Steps[1])
	}
	// Retry policy copied.
	if dup.MaxRetries != 3 || dup.RetryBackoffMinutes != 5 {
		t.Errorf("retry policy = %d/%d, want 3/5", dup.MaxRetries, dup.RetryBackoffMinutes)
	}
	// New ID.
	if dup.ID == src.ID {
		t.Errorf("duplicate has same ID as source: %d", dup.ID)
	}

	// Source flow is untouched.
	orig, _ := e.server.Store.GetFlow(ctx, src.ID)
	if orig.Name != "dup-source" {
		t.Errorf("source name changed to %q", orig.Name)
	}
}

// TestFlowDuplicateCustomName uses the optional name in the request body.
func TestFlowDuplicateCustomName(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	src, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "custom-src",
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/flows/"+itoa(src.ID)+"/duplicate", adminTok,
		map[string]any{"name": "my-custom-copy"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var dup model.Flow
	json.Unmarshal(body, &dup)
	if dup.Name != "my-custom-copy" {
		t.Errorf("name = %q, want my-custom-copy", dup.Name)
	}
}

// TestFlowDuplicateMissingFlow returns 404 for a nonexistent flow ID.
func TestFlowDuplicateMissingFlow(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	resp, body := doJSON(t, "POST", ts.URL+"/api/flows/999999/duplicate", adminTok, map[string]any{})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

// TestFlowDuplicateRejectsNonAdmin verifies the endpoint is admin-only.
func TestFlowDuplicateRejectsNonAdmin(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	src, err := e.server.Store.CreateFlow(ctx, &model.Flow{
		Name:  "auth-src",
		Steps: []model.Step{{Type: model.StepEncode}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, _ := doJSON(t, "POST", ts.URL+"/api/flows/"+itoa(src.ID)+"/duplicate", "", map[string]any{})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", resp.StatusCode)
	}
}

// TestFlowDuplicateNameCollision verifies a duplicate whose target name
// already exists returns 409 (not 500): the UNIQUE-violation branch.
func TestFlowDuplicateNameCollision(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)
	ctx := ctxBg()

	if _, err := e.server.Store.CreateFlow(ctx, &model.Flow{Name: "taken-name", Steps: []model.Step{{Type: model.StepMux}}}); err != nil {
		t.Fatal(err)
	}
	src, err := e.server.Store.CreateFlow(ctx, &model.Flow{Name: "collision-src", Steps: []model.Step{{Type: model.StepEncode}}})
	if err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "POST", ts.URL+"/api/flows/"+itoa(src.ID)+"/duplicate", adminTok, map[string]any{"name": "taken-name"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409: %s", resp.StatusCode, body)
	}
}

// TestIsUniqueConstraintErr pins the error classification used by the flow
// create/duplicate handlers: only UNIQUE violations map to 409; anything
// else must fall through to 500 (review P1-1 regression guard).
func TestIsUniqueConstraintErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New(`create flow: UNIQUE constraint failed: flows.name`), true},
		{errors.New(`pq: duplicate key value violates unique constraint "flows_name_key"`), true},
		{errors.New("database disk image is malformed"), false},
		{errors.New("connection refused"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isUniqueConstraintErr(c.err); got != c.want {
			t.Errorf("isUniqueConstraintErr(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
