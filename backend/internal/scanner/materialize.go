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
// matches the type (case-insensitive — shares may be case-insensitive and
// the templates listing filters case-insensitively). Returns the template's
// base name for clarity.
func validateAssignment(sr *model.Series) error {
	if sr.ScriptType != "vpy" && sr.ScriptType != "avs" {
		return fmt.Errorf("invalid script type %q (want vpy or avs)", sr.ScriptType)
	}
	if !ValidTemplateName(sr.ScriptTemplate) {
		return fmt.Errorf("invalid template name %q", sr.ScriptTemplate)
	}
	if !strings.EqualFold(strings.TrimPrefix(path.Ext(sr.ScriptTemplate), "."), sr.ScriptType) {
		return fmt.Errorf("template %q extension does not match script type %q", sr.ScriptTemplate, sr.ScriptType)
	}
	if !ValidTemplateName(sr.Name) {
		return fmt.Errorf("invalid series name %q", sr.Name)
	}
	return nil
}

// validateCandidatePath rejects EpisodeDir/SourceFile values that could
// escape the episode folder or inject code via placeholder substitution:
// no '.'/'..' segments, no absolute paths or backslashes, no control
// characters (a newline in a source name would inject live statements into
// the materialized script).
func validateCandidatePath(c Candidate) error {
	for _, v := range []string{c.EpisodeDir, c.SourceFile} {
		if strings.ContainsAny(v, "\\\x00") || strings.HasPrefix(v, "/") {
			return fmt.Errorf("unsafe path value %q", v)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("path value contains control characters")
			}
		}
		for _, seg := range strings.Split(v, "/") {
			if seg == "." || seg == ".." {
				return fmt.Errorf("path value contains a dot segment")
			}
		}
	}
	return nil
}

// installNoReplace atomically installs tmp at target WITHOUT replacing an
// existing file: hardlink+unlink is the POSIX renameat2(RENAME_NOREPLACE)
// equivalent Go exposes portably (link fails EEXIST when target exists).
// On filesystems without hardlink support it falls back to a re-check +
// rename (residual race documented — the primary guard is the link).
func installNoReplace(tmpName, target string) error {
	err := os.Link(tmpName, target)
	if err == nil {
		return os.Remove(tmpName)
	}
	if os.IsExist(err) {
		return fmt.Errorf("target %s appeared during materialization; per-episode scripts win", target)
	}
	// Hardlinks unsupported (some SMB/FUSE mounts): re-check then rename.
	if _, statErr := os.Lstat(target); statErr == nil {
		return fmt.Errorf("target %s appeared during materialization; per-episode scripts win", target)
	}
	return os.Rename(tmpName, target)
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
	if err := validateCandidatePath(c); err != nil {
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
	// 0644: the encode agent reads the script off the share as its own
	// identity — CreateTemp's 0600 breaks cross-identity NFS/SMB access.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return Candidate{}, fmt.Errorf("chmod temp script: %w", err)
	}
	target := filepath.Join(epDir, sr.ScriptTemplate)
	if err := installNoReplace(tmpName, target); err != nil {
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
	if err := validateCandidatePath(c); err != nil {
		return Candidate{}, err
	}
	// Direct children only: the real store lists recursively, so a nested
	// .vpy must not count as the episode's script (fs guard parity).
	directNames := func(objs []model.S3Object) []string {
		var names []string
		prefix := c.EpisodeDir + "/"
		for _, o := range objs {
			rel := strings.TrimPrefix(o.Key, prefix)
			if rel != o.Key && !strings.Contains(rel, "/") {
				names = append(names, rel)
			}
		}
		return names
	}
	objs, err := store.List(ctx, bucket, c.EpisodeDir+"/")
	if err != nil {
		return Candidate{}, fmt.Errorf("list episode prefix: %w", err)
	}
	if episodeHasScript(directNames(objs)) {
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
	// Re-check immediately before the put: S3 has no atomic no-replace on
	// the minimal ObjectStore interface, so this shrinks (not eliminates)
	// the List->Put race; a hand-authored script landing in the window is
	// still refused. Residual race documented for a future PutIfAbsent.
	objs, err = store.List(ctx, bucket, c.EpisodeDir+"/")
	if err != nil {
		return Candidate{}, fmt.Errorf("re-list episode prefix: %w", err)
	}
	if episodeHasScript(directNames(objs)) {
		return Candidate{}, fmt.Errorf("episode %s already has a filter script; per-episode scripts win", c.EpisodeDir)
	}
	destKey := c.EpisodeDir + "/" + sr.ScriptTemplate
	if err := store.Put(ctx, bucket, destKey, outFile); err != nil {
		return Candidate{}, fmt.Errorf("put episode script %s: %w", destKey, err)
	}
	c.ScriptFile = sr.ScriptTemplate
	c.ScriptType = sr.ScriptType
	return c, nil
}
