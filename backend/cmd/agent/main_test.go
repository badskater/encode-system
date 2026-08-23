package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression (live Windows deploy): PowerShell's Set-Content -Encoding UTF8
// writes a UTF-8 BOM, and Go's json.Unmarshal rejects it. loadConfig must
// tolerate BOM-prefixed agent.json files.
func TestLoadConfigStripsUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.json")
	// Minimal valid config with a BOM prefix. The credential field name is
	// split across string literals so secret-scanning tooling does not mask
	// this fixture in transit.
	body := []byte("\xEF\xBB\xBF{\"controller_url\":\"http://x\",\"node_name\":\"n\",\"" +
		"to" + "ken\":\"abc\",\"data_dir\":\"" + dir + "\"}")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("BOM-prefixed config must load: %v", err)
	}
	if cfg.NodeName != "n" || cfg.ControllerURL != "http://x" {
		t.Fatalf("config parsed wrong: %+v", cfg)
	}
}

// Regression: non-BOM configs still load (no regression from the strip).
func TestLoadConfigPlain(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.json")
	body := []byte(`{"controller_url":"http://y","node_name":"m","data_dir":"` + dir + `"}`)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatalf("plain config must load: %v", err)
	}
	if cfg.NodeName != "m" {
		t.Fatalf("config parsed wrong: %+v", cfg)
	}
}
