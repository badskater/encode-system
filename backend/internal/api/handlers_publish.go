package api

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/badskater/encode-system/backend/internal/agent"
	"github.com/badskater/encode-system/backend/internal/model"
)

// ---------- Agent payload publishing (WebUI → controller → nodes) ----------

// maxPublishBytes caps each payload upload: agent ~50 MiB, lib tiny, bin
// packages up to 1 GiB (a full tools folder with x265/eac3to/etc).
const (
	maxAgentPublishBytes = 64 << 20 // 64 MiB
	maxLibPublishBytes   = 4 << 20  // 4 MiB
	maxBinPublishBytes   = 1 << 30  // 1 GiB
	// Multipart framing/version-field headroom on top of the payload cap for
	// the MaxBytesReader request-body limit.
	maxMultipartOverhead = 1 << 20 // 1 MiB
)

// handlePublishAgent uploads a new agent binary from the UI. Form fields:
// version (string, required) + file (binary). Nodes adopt it on their next
// idle heartbeat.
func (s *Server) handlePublishAgent(w http.ResponseWriter, r *http.Request) {
	// Hard request-body cap: aborts the transfer mid-stream when exceeded,
	// so an oversize upload never occupies controller disk or memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentPublishBytes+maxMultipartOverhead)
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
		// The store enforces the version rule under its lock; a collision is
		// a client error, not a server fault.
		if strings.Contains(err.Error(), "already published") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "publish agent")
		return
	}
	s.Log.Info("agent binary published from UI", "version", version, "bytes", hdr.Size)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// handlePublishLib uploads a new EncodeLib.ps1. Form fields: version (int
// counter, must exceed the current one) + file.
func (s *Server) handlePublishLib(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxLibPublishBytes+maxMultipartOverhead)
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
	// Strict-increase is enforced inside the store under its lock (a
	// pre-check here would race concurrent publishes).
	if err := s.Update.PublishLib(version, f); err != nil {
		if strings.Contains(err.Error(), "must exceed") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
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
	r.Body = http.MaxBytesReader(w, r.Body, maxBinPublishBytes+maxMultipartOverhead)
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
	if err := validateBinZip(bytes.NewReader(body.Bytes()), int64(len(body.Bytes()))); err != nil {
		writeErr(w, http.StatusBadRequest, "bin package rejected: "+err.Error())
		return
	}
	if err := s.Update.PublishBin(version, bytes.NewReader(body.Bytes())); err != nil {
		if strings.Contains(err.Error(), "must exceed") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "publish bin")
		return
	}
	s.Log.Info("bin package published from UI", "version", version, "bytes", hdr.Size)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// binFetchClient downloads bin packages from URLs. Generous timeout: a 144
// MiB package over a modest uplink can legitimately take minutes.
var binFetchClient = &http.Client{Timeout: 10 * time.Minute}

// handlePublishBinFromURL fetches a bin package from a URL (e.g. a GitHub
// release asset on badskater/encode-bin), validates it exactly like a
// browser upload, and publishes it. JSON body: {"url": "...", "version": N,
// "sha256": "optional hex digest"}. The download streams to a temp file
// (never memory) under the same 1 GiB cap as uploads; when a sha256 is
// given, a mismatch fails the publish BEFORE the package is stored.
func (s *Server) handlePublishBinFromURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string `json:"url"`
		Version int64  `json:"version"`
		SHA256  string `json:"sha256"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.Version <= 0 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	// Scheme guard: only http(s). Anything else (file://, gopher://, …) is
	// refused outright — an admin typo should never turn into a local read.
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme == "" {
		writeErr(w, http.StatusBadRequest, "url is not valid")
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		writeErr(w, http.StatusBadRequest, "url must be http(s)")
		return
	}

	// Download to a temp file with a hard size ceiling (streaming — a 144
	// MiB package never sits in controller memory).
	tmp, err := os.CreateTemp("", "encode-bin-fetch-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create temp file")
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // best-effort cleanup on every exit path
	defer tmp.Close()

	fresp, err := binFetchClient.Get(req.URL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "fetch failed: "+err.Error())
		return
	}
	defer fresp.Body.Close()
	if fresp.StatusCode != http.StatusOK {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("fetch returned HTTP %d", fresp.StatusCode))
		return
	}
	limited := io.LimitReader(fresp.Body, maxBinPublishBytes+1)
	n, err := io.Copy(tmp, limited)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download failed: "+err.Error())
		return
	}
	if n > maxBinPublishBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "bin package too large")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, "rewind temp file")
		return
	}

	// Optional integrity gate: when the operator pins a digest (release
	// notes carry it), a mismatch aborts before anything is stored.
	if want := strings.ToLower(strings.TrimSpace(req.SHA256)); want != "" {
		h := sha256.New()
		if _, err := io.Copy(h, io.TeeReader(tmp, io.Discard)); err != nil {
			writeErr(w, http.StatusInternalServerError, "hash download")
			return
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("sha256 mismatch: got %s, want %s", got, want))
			return
		}
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			writeErr(w, http.StatusInternalServerError, "rewind temp file")
			return
		}
	}

	if err := validateBinZip(tmp, n); err != nil {
		writeErr(w, http.StatusBadRequest, "bin package rejected: "+err.Error())
		return
	}
	if err := s.Update.PublishBin(req.Version, tmp); err != nil {
		if strings.Contains(err.Error(), "must exceed") {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "publish bin")
		return
	}
	s.Log.Info("bin package published from URL", "version", req.Version, "bytes", n, "url", req.URL)
	writeJSON(w, http.StatusOK, s.Update.Manifest())
}

// validateBinZip checks a package: must be a readable zip, every entry must
// be a plain relative path (zip-slip guard: absolute, UNC, and drive-letter
// paths rejected), no symlinks, sane entry count. Rejecting here protects
// every node that would otherwise extract it. Takes any ReaderAt + size so
// both in-memory uploads and streamed temp files share one code path.
func validateBinZip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
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
		if zf.FileInfo().Mode()&0o170000 == 0o120000 { // symlink
			return fmt.Errorf("symlink entry not allowed: %q", zf.Name)
		}
		if err := agent.SafeRelPath(strings.ReplaceAll(zf.Name, `\`, "/")); err != nil {
			return fmt.Errorf("entry %q: %w", zf.Name, err)
		}
	}
	return nil
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
