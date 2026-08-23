package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Step templates ----------

// templateKeyRe constrains template keys to safe identifiers usable as
// PowerShell function suffixes and URL path segments.
var templateKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,47}$`)

// psFuncNameRe extracts the function name from a PowerShell function header.
var psFuncNameRe = regexp.MustCompile(`(?m)^\s*function\s+([A-Za-z][A-Za-z0-9_-]*)`)

// handleListStepTemplates returns all templates (built-ins + custom).
func (s *Server) handleListStepTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := s.Store.ListStepTemplates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list templates")
		return
	}
	writeJSON(w, http.StatusOK, templates)
}

// handleCreateStepTemplate validates and stores a custom step template.
// Validation: key shape, non-empty PS, function header present. When pwsh is
// available the script is parsed for syntax errors before accepting it.
func (s *Server) handleCreateStepTemplate(w http.ResponseWriter, r *http.Request) {
	var t model.StepTemplate
	if err := decodeJSON(r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid template")
		return
	}
	if err := validateTemplate(&t, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg, ok := s.parsePowerShell(t.PowerShell); !ok {
		writeErr(w, http.StatusBadRequest, "powershell: "+msg)
		return
	}
	t.Builtin = false
	created, err := s.Store.UpsertStepTemplate(r.Context(), &t)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create template: "+err.Error())
		return
	}
	s.Log.Info("step template created", "key", created.Key)
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateStepTemplate edits a template. Built-ins may be edited (their
// PowerShell is admin-owned) but never renamed or converted to custom.
func (s *Server) handleUpdateStepTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad template id")
		return
	}
	existing, err := s.Store.GetStepTemplate(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	var req model.StepTemplate
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid template")
		return
	}
	// Key and builtin-ness are immutable on update.
	req.Key = existing.Key
	req.Builtin = existing.Builtin
	if req.Label == "" {
		req.Label = existing.Label
	}
	if req.PowerShell == "" {
		req.PowerShell = existing.PowerShell
	}
	if req.Params == nil {
		req.Params = existing.Params
	}
	if err := validateTemplate(&req, true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Updates must still define a function — the renderer invokes whatever
	// the template declares, and a function-less script would panic render.
	if psFuncNameRe.FindStringSubmatch(req.PowerShell) == nil {
		writeErr(w, http.StatusBadRequest, "powershell must define a function")
		return
	}
	if msg, ok := s.parsePowerShell(req.PowerShell); !ok {
		writeErr(w, http.StatusBadRequest, "powershell: "+msg)
		return
	}
	updated, err := s.Store.UpsertStepTemplate(r.Context(), &req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "update template: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteStepTemplate removes a custom template (store enforces the
// built-in and in-use guards).
func (s *Server) handleDeleteStepTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad template id")
		return
	}
	if _, err := s.Store.GetStepTemplate(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	if err := s.Store.DeleteStepTemplate(r.Context(), id); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validateTemplate checks the fields every template must satisfy.
func validateTemplate(t *model.StepTemplate, isUpdate bool) error {
	if !templateKeyRe.MatchString(t.Key) {
		return fmt.Errorf("key must match %s", templateKeyRe.String())
	}
	if t.Label == "" {
		return fmt.Errorf("label required")
	}
	if t.PowerShell == "" {
		return fmt.Errorf("powershell required")
	}
	if !isUpdate {
		if m := psFuncNameRe.FindStringSubmatch(t.PowerShell); m == nil {
			return fmt.Errorf("powershell must define a function")
		}
	}
	return nil
}

// parsePowerShell syntax-checks a script with pwsh when available. Returns
// (message, ok). Missing pwsh is not an error — validation is best-effort on
// controllers without PowerShell installed.
func (s *Server) parsePowerShell(script string) (string, bool) {
	ps, err := findPwsh()
	if err != nil {
		return "", true // no pwsh: accept without syntax check
	}
	dir, err := os.MkdirTemp("", "encode-ps-check-")
	if err != nil {
		return "", true
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "check.ps1")
	if err := os.WriteFile(p, []byte(script), 0o600); err != nil {
		return "", true
	}
	checkerPath := filepath.Join(dir, "checker.ps1")
	checker := `$target = $args[0]
$t = $null; $e = $null
[System.Management.Automation.Language.Parser]::ParseFile($target, [ref]$t, [ref]$e) | Out-Null
if ($e.Count) { $e | ForEach-Object { Write-Output $_.Message }; exit 1 }
`
	if err := os.WriteFile(checkerPath, []byte(checker), 0o600); err != nil {
		return "", true
	}
	cmd := exec.Command(ps, "-NoProfile", "-File", checkerPath, p)
	cmd.Env = append(os.Environ(), "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), false
	}
	return "", true
}

// findPwsh locates a PowerShell interpreter for syntax validation.
func findPwsh() (string, error) {
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p, nil
	}
	for _, c := range []string{
		`C:\Program Files\PowerShell\7\pwsh.exe`,
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no powershell found")
}

// ---------- Pairing codes ----------

// pairingCodeRe is the accepted code alphabet check (hex, 32+ chars).
var pairingCodeRe = regexp.MustCompile(`^[0-9a-f]{32,128}$`)

// handleCreatePairingCode issues a one-shot pairing code shown once in the UI.
func (s *Server) handleCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NameHint string `json:"name_hint"`
		TTLHours int    `json:"ttl_hours"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	ttl := time.Duration(req.TTLHours) * time.Hour
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 1 * time.Hour
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		writeErr(w, http.StatusInternalServerError, "generate code")
		return
	}
	code := hex.EncodeToString(b)
	pc, err := s.Store.CreatePairingCode(r.Context(), auth.HashToken(code), req.NameHint, ttl)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store code")
		return
	}
	s.Log.Info("pairing code issued", "id", pc.ID, "hint", req.NameHint)
	// The plaintext code leaves the controller exactly once, right here.
	writeJSON(w, http.StatusCreated, map[string]any{"pairing": pc, "code": code})
}

// handleListPairingCodes shows active codes (hashes only — never plaintext).
func (s *Server) handleListPairingCodes(w http.ResponseWriter, r *http.Request) {
	codes, err := s.Store.ListPairingCodes(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list codes")
		return
	}
	writeJSON(w, http.StatusOK, codes)
}

// handleAgentPair registers a node with a one-shot pairing code. The agent
// presents code + desired node name; the controller creates the node, issues
// its permanent token, and consumes the code. This endpoint intentionally
// requires no bearer auth — the code IS the credential.
func (s *Server) handleAgentPair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid pairing request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "node name required (max 64 chars)")
		return
	}
	if !pairingCodeRe.MatchString(req.Code) {
		writeErr(w, http.StatusBadRequest, "invalid pairing code format")
		return
	}
	ctx := r.Context()

	// Validate the code BEFORE creating any node: an invalid/expired code
	// must not leave residue, and a transient DB error must not be
	// misreported as an auth failure with a rolled-back node.
	codeHash := auth.HashToken(req.Code)
	if _, err := s.Store.PeekPairingCode(ctx, codeHash); err != nil {
		writeErr(w, http.StatusUnauthorized, "pairing code rejected: "+err.Error())
		return
	}

	nodeToken, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate token")
		return
	}
	node, err := s.Store.CreateNode(ctx, req.Name, auth.HashToken(nodeToken))
	if err != nil {
		writeErr(w, http.StatusConflict, "node name already registered")
		return
	}
	pc, err := s.Store.ConsumePairingCode(ctx, codeHash, node.ID)
	if err != nil {
		// Extremely unlikely (code validated above); roll back and surface
		// the real error rather than an auth failure.
		if derr := s.Store.DeleteNode(ctx, node.ID); derr != nil {
			s.Log.Error("rollback node after failed pairing", "err", derr, "node", req.Name)
		}
		writeErr(w, http.StatusConflict, "pairing failed: "+err.Error())
		return
	}
	s.Log.Info("node paired", "node", req.Name, "pairing_id", pc.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"node":  node,
		"token": nodeToken,
	})
}
