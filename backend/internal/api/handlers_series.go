package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/badskater/encode-system/backend/internal/flow"
	"github.com/badskater/encode-system/backend/internal/model"
)

// validateSeriesName rejects names that could escape the share root or break
// Windows filenames. Mirrors Assert-SafeName in EncodeLib.ps1 (the agent-side
// guard rendered scripts pass through) so anything accepted here is also safe
// inside rendered jobs. Empty is invalid; max 200 bytes keeps folder paths
// sane.
func validateSeriesName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 200 {
		return fmt.Errorf("name too long (max 200 characters)")
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("name must not contain '..'")
	}
	if strings.ContainsAny(name, "\\/:*?\"<>|") {
		return fmt.Errorf("name contains characters not allowed in folder names")
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("name must not start with '.' or end with a space or dot")
	}
	return nil
}

// episodeFolderName renders the canonical episode folder name ("Ep 01"). The
// zero-padding width follows the episode count so >99-episode series stay
// sortable ("Ep 001") while normal series keep the legacy two-digit form.
func episodeFolderName(i, episodes int) string {
	width := 2
	for n := episodes; n >= 100; n /= 10 {
		width++
	}
	return fmt.Sprintf("Ep %0*d", width, i)
}

// handleCreateSeries builds a new series' folder structure on the mounted
// shares and registers it:
//   - scripts share: <ScriptsRoot>/<Name>/Ep 01 … Ep NN (empty — the operator
//     drops in sources + filter scripts; the scanner queues jobs once both
//     exist per episode)
//   - release share: <ReleaseRoot>/[Group] <Name> - Raws [Tag] (the "Last"
//     destination release_copy writes into)
//
// Idempotent: existing folders are kept, missing episode folders are added,
// so re-running with a higher episode count extends the series.
func (s *Server) handleCreateSeries(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Episodes int    `json:"episodes"`
		Tag      string `json:"tag"`     // optional; "" = inherit the global settings tag
		FlowID   int64  `json:"flow_id"` // optional; 0 = default flow
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Tag = strings.TrimSpace(req.Tag)
	if err := validateSeriesName(req.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Episodes < 1 || req.Episodes > 999 {
		writeErr(w, http.StatusBadRequest, "episodes must be between 1 and 999")
		return
	}
	if req.Tag != "" {
		if err := validateSeriesName(req.Tag); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid tag: "+err.Error())
			return
		}
	}

	ctx := r.Context()
	st := s.currentSettings(ctx)
	if st.ScriptsRoot == "" {
		writeErr(w, http.StatusBadRequest, "scripts root is not configured (Settings page)")
		return
	}

	// Scripts share: <root>/<Name>/Ep NN. MkdirAll is idempotent; each
	// episode folder starts empty by design (filter scripts are hand-authored
	// and the scanner only queues episodes that have source + script).
	seriesDir := filepath.Join(st.ScriptsRoot, req.Name)
	epDirs := make([]string, 0, req.Episodes)
	for i := 1; i <= req.Episodes; i++ {
		epDirs = append(epDirs, episodeFolderName(i, req.Episodes))
	}
	for _, ep := range epDirs {
		if err := os.MkdirAll(filepath.Join(seriesDir, ep), 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "create episode folder "+ep+": "+err.Error())
			return
		}
	}

	// Release share: the "Last" folder release_copy targets. Created when the
	// release root is configured; an unset root is reported, not fatal (the
	// series itself is still usable).
	tag := req.Tag
	if tag == "" {
		tag = st.Tag
	}
	releaseFolder := ""
	if st.ReleaseRoot != "" {
		releaseFolder = filepath.Join(st.ReleaseRoot, flow.ReleaseFolderName(st.Group, req.Name, tag))
		if err := os.MkdirAll(releaseFolder, 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, "create release folder: "+err.Error())
			return
		}
	}

	// Register the series (idempotent upsert), then apply the request's
	// tag/flow with field-scoped updates so a re-run only touches what the
	// operator asked for.
	sr, err := s.Store.UpsertSeriesByName(ctx, req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "register series")
		return
	}
	if req.Tag != "" {
		if err := s.Store.SetSeriesTag(ctx, sr.ID, req.Tag); err != nil {
			writeErr(w, http.StatusInternalServerError, "set series tag")
			return
		}
	}
	if req.FlowID != 0 {
		if _, err := s.Store.GetFlow(ctx, req.FlowID); err != nil {
			writeErr(w, http.StatusBadRequest, "flow not found")
			return
		}
		if err := s.Store.SetSeriesFlow(ctx, sr.ID, req.FlowID); err != nil {
			writeErr(w, http.StatusInternalServerError, "set series flow")
			return
		}
	}
	sr, err = s.Store.GetSeries(ctx, sr.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reload series")
		return
	}

	s.Log.Info("series created", "series", req.Name, "episodes", req.Episodes,
		"scripts_dir", seriesDir, "release_dir", releaseFolder)
	writeJSON(w, http.StatusCreated, struct {
		Series         *model.Series `json:"series"`
		ScriptsFolder  string        `json:"scripts_folder"`
		ReleaseFolder  string        `json:"release_folder"`
		EpisodeFolders []string      `json:"episode_folders"`
	}{
		Series:         sr,
		ScriptsFolder:  seriesDir,
		ReleaseFolder:  releaseFolder,
		EpisodeFolders: epDirs,
	})
}
