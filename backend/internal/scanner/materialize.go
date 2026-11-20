package scanner

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/s3"
)

// TemplateDirName is the reserved top-level folder on the scripts share
// holding per-series filter-script templates: _templates/<Series>/<file>.
// It is excluded from series scanning (fs + S3) and refused as a series
// name by the API.
const TemplateDirName = "_templates"

// ValidTemplateName accepts a plain base file name only: no path
// separators, no ".." anywhere, no leading dot, non-empty. The template
// name comes from operator input (API) and is joined into a filesystem
// path, so traversal must be impossible by construction.
func ValidTemplateName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	return true
}

// RenderTemplate substitutes the four documented placeholders in a script
// template. Unknown {{...}} tokens are left intact so literal braces in
// filter scripts survive. Pure function — shared by the fs and S3 paths.
func RenderTemplate(content []byte, seriesName, episodeDir, sourceFile string) []byte {
	s := string(content)
	s = strings.ReplaceAll(s, "{{series}}", seriesName)
	s = strings.ReplaceAll(s, "{{episode}}", path.Base(episodeDir))
	s = strings.ReplaceAll(s, "{{episode_dir}}", episodeDir)
	s = strings.ReplaceAll(s, "{{source}}", sourceFile)
	return []byte(s)
}

// validateAssignment checks the series' script assignment before any IO:
// type must be vpy/avs, the template a safe base name whose extension
// matches the type. Returns the template's base name for clarity.
func validateAssignment(sr *model.Series) error {
	if sr.ScriptType != "vpy" && sr.ScriptType != "avs" {
		return fmt.Errorf("invalid script type %q (want vpy or avs)", sr.ScriptType)
	}
	if !ValidTemplateName(sr.ScriptTemplate) {
		return fmt.Errorf("invalid template name %q", sr.ScriptTemplate)
	}
	if strings.TrimPrefix(path.Ext(sr.ScriptTemplate), ".") != sr.ScriptType {
		return fmt.Errorf("template %q extension does not match script type %q", sr.ScriptTemplate, sr.ScriptType)
	}
	if !ValidTemplateName(sr.Name) {
		return fmt.Errorf("invalid series name %q", sr.Name)
	}
	return nil
}

// episodeHasScript reports whether any .avs/.vpy file exists in the episode
// directory listing (names only). A per-episode script always wins over the
// series template — materialization must never overwrite one.
func episodeHasScript(names []string) bool {
	for _, n := range names {
		switch strings.ToLower(path.Ext(n)) {
		case ".avs", ".vpy":
			return true
		}
	}
	return false
}

// MaterializeScript copies the series' template into the episode folder
// with placeholder substitution, atomically (temp file + rename). It
// refuses when the episode already carries any filter script (race guard:
// the loop only calls this for scriptless candidates, but a hand-authored
// script may land between scan and materialization). Returns the candidate
// updated with the materialized script's name and type.
func MaterializeScript(root string, sr *model.Series, c Candidate) (Candidate, error) {
	if err := validateAssignment(sr); err != nil {
		return Candidate{}, err
	}
	tmplPath := filepath.Join(root, TemplateDirName, sr.Name, sr.ScriptTemplate)
	content, err := os.ReadFile(tmplPath)
	if err != nil {
		return Candidate{}, fmt.Errorf("read template %s: %w", tmplPath, err)
	}
	epDir := filepath.Join(root, filepath.FromSlash(c.EpisodeDir))
	entries, err := os.ReadDir(epDir)
	if err != nil {
		return Candidate{}, fmt.Errorf("read episode dir %s: %w", epDir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if episodeHasScript(names) {
		return Candidate{}, fmt.Errorf("episode %s already has a filter script; per-episode scripts win", c.EpisodeDir)
	}
	rendered := RenderTemplate(content, sr.Name, c.EpisodeDir, c.SourceFile)
	tmp, err := os.CreateTemp(epDir, ".materialize-*")
	if err != nil {
		return Candidate{}, fmt.Errorf("create temp script: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(rendered); err != nil {
		tmp.Close()
		return Candidate{}, fmt.Errorf("write temp script: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Candidate{}, fmt.Errorf("close temp script: %w", err)
	}
	target := filepath.Join(epDir, sr.ScriptTemplate)
	if err := os.Rename(tmpName, target); err != nil {
		return Candidate{}, fmt.Errorf("install script %s: %w", target, err)
	}
	c.ScriptFile = sr.ScriptTemplate
	c.ScriptType = sr.ScriptType
	return c, nil
}

// MaterializeScriptS3 is the bucket-side parity of MaterializeScript: get
// the template object, render, put the episode object. The overwrite guard
// lists the episode prefix for any existing .avs/.vpy key first. Get/Put
// are file-based on the ObjectStore interface, so temp files bridge them.
func MaterializeScriptS3(ctx context.Context, store s3.ObjectStore, bucket string, sr *model.Series, c Candidate) (Candidate, error) {
	if err := validateAssignment(sr); err != nil {
		return Candidate{}, err
	}
	objs, err := store.List(ctx, bucket, c.EpisodeDir+"/")
	if err != nil {
		return Candidate{}, fmt.Errorf("list episode prefix: %w", err)
	}
	names := make([]string, 0, len(objs))
	for _, o := range objs {
		names = append(names, path.Base(o.Key))
	}
	if episodeHasScript(names) {
		return Candidate{}, fmt.Errorf("episode %s already has a filter script; per-episode scripts win", c.EpisodeDir)
	}
	tmplKey := TemplateDirName + "/" + sr.Name + "/" + sr.ScriptTemplate
	tmpDir, err := os.MkdirTemp("", "encode-materialize-")
	if err != nil {
		return Candidate{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	tmplFile := filepath.Join(tmpDir, "template")
	if err := store.Get(ctx, bucket, tmplKey, tmplFile); err != nil {
		return Candidate{}, fmt.Errorf("get template %s: %w", tmplKey, err)
	}
	content, err := os.ReadFile(tmplFile)
	if err != nil {
		return Candidate{}, fmt.Errorf("read template: %w", err)
	}
	rendered := RenderTemplate(content, sr.Name, c.EpisodeDir, c.SourceFile)
	outFile := filepath.Join(tmpDir, "rendered")
	if err := os.WriteFile(outFile, rendered, 0o644); err != nil {
		return Candidate{}, fmt.Errorf("write rendered script: %w", err)
	}
	destKey := c.EpisodeDir + "/" + sr.ScriptTemplate
	if err := store.Put(ctx, bucket, destKey, outFile); err != nil {
		return Candidate{}, fmt.Errorf("put episode script %s: %w", destKey, err)
	}
	c.ScriptFile = sr.ScriptTemplate
	c.ScriptType = sr.ScriptType
	return c, nil
}
