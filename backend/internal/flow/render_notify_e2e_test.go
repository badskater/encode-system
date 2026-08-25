package flow

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/badskater/encode-system/backend/internal/model"
)

// TestDiscordNotifyPostsToWebhook runs the discord_notify step under pwsh
// against a real local mock webhook: the step must POST a JSON content
// payload carrying the series/episode identity. Also covers the best-effort
// contract: an unreachable webhook warns and the job still completes.
func TestDiscordNotifyPostsToWebhook(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell integration test")
	}

	// Mock webhook: capture every received body.
	var mu sync.Mutex
	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 1<<16)
		n, _ := r.Body.Read(buf)
		mu.Lock()
		received = append(received, string(buf[:n]))
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer srv.Close()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	scriptsDir := filepath.Join(root, "scripts")
	releaseDir := filepath.Join(root, "release")
	for _, d := range []string{binDir, scriptsDir, releaseDir} {
		os.MkdirAll(d, 0o755)
	}
	epDir := filepath.Join(scriptsDir, "Notify Series", "Ep 07")
	os.MkdirAll(epDir, 0o755)

	run := func(name string, f *model.Flow, vars Vars) string {
		j := &model.Job{ID: 300, Series: "Notify Series", Episode: "07",
			EpisodeDir: "Notify Series/Ep 07", ScriptType: "vpy"}
		script, err := Render(f, j, vars, nil)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		scriptPath := filepath.Join(root, name+".ps1")
		if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		libPath, _ := filepath.Abs("../../../powershell/EncodeLib.ps1")
		cmd := exec.Command(pwsh, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-LibPath", libPath)
		cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed under pwsh: %v\n%s", name, err, out)
		}
		return string(out)
	}
	baseVars := Vars{BinDir: binDir, ScriptsDir: scriptsDir, ReleaseDir: releaseDir,
		Group: "OldFartsSubs", Tag: "1080p"}

	// Run 1: webhook from the flow PARAM -> the mock server must receive it.
	text := run("notify-param", &model.Flow{Name: "notify-param", Steps: []model.Step{
		{Type: model.StepType("discord_notify"), Params: map[string]string{
			"webhook": srv.URL + "/api/webhooks/123/abc",
			"message": "video + audio done",
		}},
	}}, baseVars)
	for _, want := range []string{"ENCODE_STEP discord_notify", "[discord] message posted", "ENCODE_JOB_DONE"} {
		if !strings.Contains(text, want) {
			t.Errorf("param run missing %q:\n%s", want, text)
		}
	}
	mu.Lock()
	got := received
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 webhook post, got %d", len(got))
	}
	var payload struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(got[0]), &payload); err != nil {
		t.Fatalf("webhook body not JSON: %v: %s", err, got[0])
	}
	for _, want := range []string{"Notify Series", "Ep 07", "video + audio done", "Encode progress"} {
		if !strings.Contains(payload.Content, want) {
			t.Errorf("webhook content missing %q: %s", want, payload.Content)
		}
	}

	// Run 2: param blank -> falls back to the controller webhook in Vars.
	text2 := run("notify-fallback", &model.Flow{Name: "notify-fallback", Steps: []model.Step{
		{Type: model.StepType("discord_notify")},
	}}, func() Vars {
		v := baseVars
		v.DiscordWebhook = srv.URL + "/api/webhooks/456/def"
		return v
	}())
	if !strings.Contains(text2, "[discord] message posted") {
		t.Errorf("fallback run missing post confirmation:\n%s", text2)
	}
	mu.Lock()
	if len(received) != 2 {
		t.Fatalf("expected 2 webhook posts after fallback run, got %d", len(received))
	}
	mu.Unlock()

	// Run 3: nothing configured anywhere -> polite skip, job completes.
	text3 := run("notify-skip", &model.Flow{Name: "notify-skip", Steps: []model.Step{
		{Type: model.StepType("discord_notify")},
	}}, baseVars)
	for _, want := range []string{"no webhook configured", "ENCODE_JOB_DONE"} {
		if !strings.Contains(text3, want) {
			t.Errorf("skip run missing %q:\n%s", want, text3)
		}
	}

	// Run 4: unreachable webhook -> WARNING, but the job still completes
	// (best-effort contract — a webhook outage never fails an encode).
	text4 := run("notify-fail", &model.Flow{Name: "notify-fail", Steps: []model.Step{
		{Type: model.StepType("discord_notify"), Params: map[string]string{
			"webhook": "http://127.0.0.1:1/api/webhooks/dead/beef",
		}},
	}}, baseVars)
	for _, want := range []string{"WARNING: notification failed", "ENCODE_JOB_DONE"} {
		if !strings.Contains(text4, want) {
			t.Errorf("fail run missing %q:\n%s", want, text4)
		}
	}

	// Run 5: a non-Discord, non-loopback URL is refused outright (exfil
	// guard) — the step skips it instead of posting job details there.
	text5 := run("notify-guard", &model.Flow{Name: "notify-guard", Steps: []model.Step{
		{Type: model.StepType("discord_notify"), Params: map[string]string{
			"webhook": "http://evil.example.com/collect",
		}},
	}}, baseVars)
	if !strings.Contains(text5, "does not look like a Discord webhook URL") {
		t.Errorf("guard run must refuse the foreign URL:\n%s", text5)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("guard/skip/fail runs must not post (still 2, got %d)", len(received))
	}
}
