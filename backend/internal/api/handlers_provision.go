package api

import (
	"net/http"
	"strconv"

	"github.com/badskater/encode-system/backend/internal/model"
	"github.com/badskater/encode-system/backend/internal/provision"
)

// ---------- Node provisioning (controller-driven Ansible) ----------

// provisionRequest is the UI form payload. The WinRM password is used for
// this single run and never persisted.
type provisionRequest struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Scheme           string `json:"scheme"`
	WinRMUser        string `json:"winrm_user"`
	WinRMPassword    string `json:"winrm_password"`
	NodeName         string `json:"node_name"`
	InstallToolchain bool   `json:"install_toolchain"`
	MountNFS         bool   `json:"mount_nfs"`
	PushBin          bool   `json:"push_bin"`
}

// handleStartProvision validates and launches a provisioning run.
func (s *Server) handleStartProvision(w http.ResponseWriter, r *http.Request) {
	if s.Provision == nil {
		writeErr(w, http.StatusServiceUnavailable, "provisioning not available on this controller")
		return
	}
	var req provisionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	run, err := s.Provision.Start(r.Context(), provision.Request{
		Host: req.Host, Port: req.Port, Scheme: req.Scheme,
		WinRMUser: req.WinRMUser, WinRMPassword: req.WinRMPassword,
		NodeName: req.NodeName, InstallToolchain: req.InstallToolchain,
		MountNFS: req.MountNFS, PushBin: req.PushBin,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Log.Info("provision run started", "run", run.ID, "host", req.Host, "node", req.NodeName)
	writeJSON(w, http.StatusAccepted, run)
}

// handleListProvisionRuns returns recent runs (no logs — the list stays small).
func (s *Server) handleListProvisionRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ListProvisionRuns(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list provision runs")
		return
	}
	if runs == nil {
		runs = []*model.ProvisionRun{}
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleGetProvisionRunLog returns one run including its full log.
func (s *Server) handleGetProvisionRunLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad run id")
		return
	}
	run, err := s.Store.GetProvisionRun(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, run)
}
