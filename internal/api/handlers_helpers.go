package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/helpermgr"
	"proxback/internal/nodedeploy"
	"proxback/internal/store"
)

type helperDTO struct {
	ID string `json:"id"`
	// HostID and HostName name the Proxmox host the helper serves. They are
	// empty for an "unassigned" helper — one registered before helpers carried a
	// host identity — which is never used to route traffic.
	HostID       string     `json:"hostId"`
	HostName     string     `json:"hostName"`
	Node         string     `json:"node"`
	Address      string     `json:"address"`
	Port         int        `json:"port"`
	Version      string     `json:"version"`
	Status       string     `json:"status"`
	LastSeen     *time.Time `json:"lastSeen"`
	RegisteredAt time.Time  `json:"registeredAt"`
}

func toHelperDTO(h *store.NodeHelper, hostNames map[string]string) helperDTO {
	return helperDTO{
		ID: h.ID, HostID: h.HostID, HostName: hostNames[h.HostID],
		Node: h.Node, Address: h.Address, Port: h.Port, Version: h.Version,
		Status: helpermgr.Status(h), LastSeen: h.LastSeen, RegisteredAt: h.RegisteredAt,
	}
}

// hostNames maps host ids to their display names, for the DTOs that show a
// workload's or a helper's cluster.
func (s *Server) hostNames(r *http.Request) (map[string]string, error) {
	hosts, err := s.st.ListPVEHosts(r.Context())
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(hosts))
	for _, h := range hosts {
		out[h.ID] = h.Name
	}
	return out, nil
}

func (s *Server) handleListHelpers(w http.ResponseWriter, r *http.Request) {
	helpers, err := s.st.ListHelpers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	names, err := s.hostNames(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]helperDTO, 0, len(helpers))
	for _, h := range helpers {
		out = append(out, toHelperDTO(h, names))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateHelperEnrollToken mints an enrollment token for one Proxmox host.
// The host is required: the token is what tells the registering helper which
// cluster its node belongs to, and a helper without one cannot be routed to.
func (s *Server) handleCreateHelperEnrollToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HostID string `json:"hostId"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tok, err := s.helpers.CreateEnrollToken(r.Context(), body.HostID)
	if err != nil {
		if errors.Is(err, helpermgr.ErrNoHost) {
			writeError(w, http.StatusBadRequest,
				"hostId is required and must name a configured Proxmox host: the enrollment token "+
					"carries it, so the node helper inherits the cluster it belongs to")
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": tok.Token, "expiresAt": tok.ExpiresAt})
}

// handleAssignHelper binds an unassigned (or misassigned) helper to a Proxmox
// host, so an installation that predates host identity can be made routable
// without redeploying it.
func (s *Server) handleAssignHelper(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HostID string `json:"hostId"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	hostID := strings.TrimSpace(body.HostID)
	if hostID == "" {
		writeError(w, http.StatusBadRequest, "hostId is required")
		return
	}
	if _, err := s.st.PVEHostByID(r.Context(), hostID); err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.st.AssignHelperHost(r.Context(), id, hostID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "node helper not found")
			return
		}
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	h, err := s.st.HelperByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "node helper")
		return
	}
	names, err := s.hostNames(r)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.log.Info("node helper assigned to a host", "helper", id, "hostId", hostID, "node", h.Node)
	writeJSON(w, http.StatusOK, toHelperDTO(h, names))
}

func (s *Server) handleDeleteHelper(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteHelper(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "node helper")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- deployment

// helperBinaryName is the staged node helper binary in <data>/downloads. It is
// both what /downloads serves and what the SSH deployment uploads.
const helperBinaryName = "proxback-helper-linux-amd64"

// deployHelperRequest is the body of POST /api/helpers/deploy. The password is
// used for one SSH connection and is never stored or logged.
type deployHelperRequest struct {
	// HostID is the Proxmox host the node belongs to. It is required because the
	// enrollment token minted here carries it: without it the helper would
	// register unassigned and could not be used.
	HostID             string `json:"hostId"`
	Node               string `json:"node"`
	Address            string `json:"address"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	ServerURL          string `json:"serverUrl"`
	HelperPort         int    `json:"helperPort"`
	HostKeyFingerprint string `json:"hostKeyFingerprint"`
}

// handleDeployHelper installs the node helper on a PVE node over SSH: the
// operator supplies credentials once and the server does the work the install
// one-liner would otherwise do by hand.
func (s *Server) handleDeployHelper(w http.ResponseWriter, r *http.Request) {
	var body deployHelperRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	hostID := strings.TrimSpace(body.HostID)
	node := strings.TrimSpace(body.Node)
	address := strings.TrimSpace(body.Address)
	username := strings.TrimSpace(body.Username)
	serverURL := strings.TrimSpace(body.ServerURL)
	switch {
	case hostID == "":
		writeError(w, http.StatusBadRequest,
			"hostId is required: a node helper is identified by the Proxmox host it belongs to "+
				"together with its node name")
		return
	case node == "":
		writeError(w, http.StatusBadRequest, "node is required")
		return
	case address == "":
		writeError(w, http.StatusBadRequest, "address is required")
		return
	case username == "":
		writeError(w, http.StatusBadRequest, "username is required")
		return
	case body.Password == "":
		writeError(w, http.StatusBadRequest, "password is required")
		return
	case serverURL == "":
		writeError(w, http.StatusBadRequest, "serverUrl is required")
		return
	}
	if err := validateServerURL(serverURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.st.PVEHostByID(r.Context(), hostID); err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	port := body.Port
	if port <= 0 {
		port = nodedeploy.DefaultSSHPort
	}
	helperPort := body.HelperPort
	if helperPort <= 0 {
		helperPort = store.DefaultHelperPort
	}

	binary := filepath.Join(s.dataDir, "downloads", helperBinaryName)
	if st, err := os.Stat(binary); err != nil || st.IsDir() {
		writeError(w, http.StatusBadRequest,
			"the node helper binary is not staged on this server; build it and place it in <data>/downloads/"+helperBinaryName)
		return
	}

	ctx := r.Context()
	// The token is minted here rather than by the operator: it only ever
	// travels to the node on the remote command line. It is single use, carries
	// the host the helper will belong to, and expires in 24 h, so one left
	// unused by a failed deployment is harmless.
	tok, err := s.helpers.CreateEnrollToken(ctx, hostID)
	if err != nil {
		if errors.Is(err, helpermgr.ErrNoHost) {
			writeError(w, http.StatusBadRequest, "hostId must name a configured Proxmox host")
			return
		}
		s.serverError(w, err)
		return
	}
	res, err := s.deployHelper(ctx, nodedeploy.Params{
		Address:             address,
		Port:                port,
		Username:            username,
		Password:            body.Password,
		ExpectedFingerprint: strings.TrimSpace(body.HostKeyFingerprint),
		BinaryPath:          binary,
		ServerURL:           serverURL,
		EnrollToken:         tok.Token,
		HelperPort:          helperPort,
	})
	if err != nil {
		var fpErr *nodedeploy.FingerprintError
		if errors.As(err, &fpErr) {
			// Trust on first use: nothing ran, the operator confirms this
			// fingerprint and retries with it.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       "confirm the node's SSH host key fingerprint",
				"fingerprint": fpErr.Fingerprint,
			})
			return
		}
		s.log.Warn("node helper deployment failed",
			"node", node, "address", address, "port", port, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("node helper deployed", "node", node, "address", address, "port", port)

	logLines := res.Log
	if logLines == nil {
		logLines = []string{}
	}
	// The helper enrolls itself during --install, so its row normally exists by
	// now; reporting it is best effort either way. It is looked up by the pair
	// that identifies it, never by node name alone.
	online := false
	if h, err := s.st.HelperFor(ctx, hostID, node); err == nil {
		online = helpermgr.Online(h)
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("could not look up the deployed helper",
			"hostId", hostID, "node", node, "error", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "log": logLines, "helperOnline": online,
	})
}

// validateServerURL keeps the URL the helper will enroll against to something
// it can actually reach.
func validateServerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("serverUrl is not a valid URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("serverUrl must be an http:// or https:// URL")
	}
	return nil
}

// ---------------------------------------------------------------- helper facing

func (s *Server) handleHelperRegister(w http.ResponseWriter, r *http.Request) {
	var body helpermgr.RegisterRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Node) == "" || body.AccessSecret == "" {
		writeError(w, http.StatusBadRequest, "node and accessSecret are required")
		return
	}
	res, err := s.helpers.Register(r.Context(), body, r.RemoteAddr)
	if err != nil {
		if errors.Is(err, helpermgr.ErrBadToken) {
			writeError(w, http.StatusUnauthorized, "invalid or expired enrollment token")
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHelperHeartbeat(w http.ResponseWriter, r *http.Request) {
	h := helperFrom(r.Context())
	if err := s.helpers.Heartbeat(r.Context(), h.ID); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
