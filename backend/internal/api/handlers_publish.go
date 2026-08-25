package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Agent payload publishing (WebUI → controller → nodes) ----------

// maxPublishBytes caps each payload upload: agent ~50 MiB, lib tiny, bin
// packages up to 1 GiB (a full tools folder with x265/eac3to/etc).
const (
	maxAgentPublishBytes = 64 << 20   // 64 MiB
	maxLibPublishBytes   = 4 << 20    // 4 MiB
	maxBinPublishBytes   = 1 << 30    // 1 GiB
)

// handlePublishAgent uploads a new agent binary from the UI. Form fields:
// version (string, required) + file (binary). Nodes adopt it on their next
// idle heartbeat.
func (s *Server) handlePublishAgent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxAgentPublishBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart parse: "+err.Error())
		return
	}
	version := r.FormValue("version")
	if version == "" {
		writeErr(w, http.StatusBadRequest, "version field is required")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer f.Close()
	if hdr.Size > maxAgentPublishBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "agent payload too large")
		return
	}
	if err := s.Update.PublishAgent(version, f); err != nil {
		writeErr(w, http.StatusInternalServerError, "publish agent")
		return
	}
	s.Log.Info("agent binary published from UI", "version", version, "bytes", hdr.Size)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// handlePublishLib uploads a new EncodeLib.ps1. Form fields: version (int
// counter, must exceed the current one) + file.
func (s *Server) handlePublishLib(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxLibPublishBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart parse: "+err.Error())
		return
	}
	vStr := r.FormValue("version")
	version, err := strconv.ParseInt(vStr, 10, 64)
	if err != nil || version <= 0 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	if cur := s.Update.Manifest(); version <= cur.LibVersion {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("version must exceed the current lib version (%d)", cur.LibVersion))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer f.Close()
	if hdr.Size > maxLibPublishBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "lib payload too large")
		return
	}
	if err := s.Update.PublishLib(version, f); err != nil {
		writeErr(w, http.StatusInternalServerError, "publish lib")
		return
	}
	s.Log.Info("EncodeLib published from UI", "version", version, "bytes", hdr.Size)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// handlePublishBin uploads a new tools-folder zip package. Form fields:
// version (int counter, must exceed current) + file (.zip). Nodes extract it
// over their bin dir. The zip is validated server-side (structure + no path
// traversal) before it is stored — a corrupt or malicious package is
// rejected at publish time, not on the worker.
func (s *Server) handlePublishBin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxBinPublishBytes); err != nil {
		writeErr(w, http.StatusBadRequest, "multipart parse: "+err.Error())
		return
	}
	vStr := r.FormValue("version")
	version, err := strconv.ParseInt(vStr, 10, 64)
	if err != nil || version <= 0 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	if cur := s.Update.Manifest(); version <= cur.BinVersion {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("version must exceed the current bin version (%d)", cur.BinVersion))
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer f.Close()
	if hdr.Size > maxBinPublishBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "bin package too large")
		return
	}
	// Buffer the upload once: validation reads it fully, then we publish the
	// same bytes — no seeking on the multipart handle (not guaranteed to be
	// a seeker).
	body := &bytes.Buffer{}
	if _, err := io.Copy(body, f); err != nil {
		writeErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if err := validateBinZip(bytes.NewReader(body.Bytes()), int64(body.Len())); err != nil {
		writeErr(w, http.StatusBadRequest, "bin package rejected: "+err.Error())
		return
	}
	if err := s.Update.PublishBin(version, bytes.NewReader(body.Bytes())); err != nil {
		writeErr(w, http.StatusInternalServerError, "publish bin")
		return
	}
	s.Log.Info("bin package published from UI", "version", version, "bytes", hdr.Size)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// validateBinZip checks the uploaded package: must be a readable zip, every
// entry must be a plain relative path (zip-slip guard), no symlinks, sane
// total entry count and size. Rejecting here protects every node that would
// otherwise extract it.
func validateBinZip(r io.Reader, size int64) error {
	// Read the whole zip into memory for central-directory parsing. The 1 GiB
	// cap above bounds this; typical tool folders are tens of MB.
	b := &bytes.Buffer{}
	if _, err := io.Copy(b, r); err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		return fmt.Errorf("not a valid zip archive: %w", err)
	}
	if len(zr.File) == 0 {
		return fmt.Errorf("zip is empty")
	}
	if len(zr.File) > 5000 {
		return fmt.Errorf("too many entries (%d)", len(zr.File))
	}
	for _, zf := range zr.File {
		name := zf.Name
		if name == "" {
			return fmt.Errorf("entry with empty name")
		}
		if name[0] == '/' || name[0] == '\\' {
			return fmt.Errorf("absolute path in zip: %q", name)
		}
		if zf.FileInfo().Mode()&0o170000 == 0o120000 { // symlink
			return fmt.Errorf("symlink entry not allowed: %q", name)
		}
		if containsPathTraversal(name) {
			return fmt.Errorf("path traversal in entry %q", name)
		}
	}
	return nil
}

// containsPathTraversal flags ../ segments in any separator style.
func containsPathTraversal(name string) bool {
	seps := []string{`../`, `..\`}
	for _, sep := range seps {
		for i := 0; i+len(sep) <= len(name); i++ {
			if name[i:i+len(sep)] == sep {
				return true
			}
		}
	}
	return len(name) >= 2 && name[len(name)-2:] == ".."
}

// handleDownloadBin streams the stored bin-package zip to a node.
func (s *Server) handleDownloadBin(w http.ResponseWriter, r *http.Request, _ *model.Node) {
	f, err := s.Update.BinPayload()
	if err != nil {
		writeErr(w, http.StatusNotFound, "no bin package published")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	http.ServeContent(w, r, "bin-package.zip", time.Time{}, f)
}

// handleManifestAdmin exposes the update manifest to the UI (Settings page
// shows what is currently published).
func (s *Server) handleManifestAdmin(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}
