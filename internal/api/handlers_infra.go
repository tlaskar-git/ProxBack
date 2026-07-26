package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/pve"
	"proxback/internal/s3target"
	"proxback/internal/sched"
	"proxback/internal/store"
)

// ---------------------------------------------------------------- hosts

type hostDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	BaseURL     string     `json:"baseUrl"`
	TokenID     string     `json:"tokenId"`
	InsecureTLS bool       `json:"insecureTLS"`
	Status      string     `json:"status"`
	LastSeen    *time.Time `json:"lastSeen"`
}

func toHostDTO(h *store.PVEHost) hostDTO {
	return hostDTO{
		ID: h.ID, Name: h.Name, BaseURL: h.BaseURL, TokenID: h.TokenID,
		InsecureTLS: h.InsecureTLS, Status: h.Status, LastSeen: h.LastSeen,
	}
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.st.ListPVEHosts(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]hostDTO, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, toHostDTO(h))
	}
	writeJSON(w, http.StatusOK, out)
}

type createHostRequest struct {
	Name        string `json:"name"`
	BaseURL     string `json:"baseUrl"`
	TokenID     string `json:"tokenId"`
	TokenSecret string `json:"tokenSecret"`
	InsecureTLS bool   `json:"insecureTLS"`
}

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	var body createHostRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.BaseURL = strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if body.Name == "" || body.BaseURL == "" || body.TokenID == "" || body.TokenSecret == "" {
		writeError(w, http.StatusBadRequest, "name, baseUrl, tokenId and tokenSecret are required")
		return
	}
	client, err := pve.New(pve.Config{
		BaseURL:     body.BaseURL,
		TokenID:     body.TokenID,
		TokenSecret: body.TokenSecret,
		InsecureTLS: body.InsecureTLS,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	nodes, err := client.Nodes(r.Context())
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not reach Proxmox host: "+err.Error())
		return
	}
	now := store.Now()
	host, err := s.st.CreatePVEHost(r.Context(), &store.PVEHost{
		Name: body.Name, BaseURL: body.BaseURL, TokenID: body.TokenID,
		TokenSecret: body.TokenSecret, InsecureTLS: body.InsecureTLS,
		Status: "online", LastSeen: &now,
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.log.Info("proxmox host added", "host", host.Name, "nodes", len(nodes))
	if _, err := s.refreshHostVMs(r, host); err != nil {
		s.log.Warn("could not refresh vm inventory", "host", host.Name, "error", err)
	}
	writeJSON(w, http.StatusOK, toHostDTO(host))
}

func (s *Server) handleTestHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.st.PVEHostByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	client, err := sched.PVEClient(host)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "nodes": 0, "error": err.Error()})
		return
	}
	nodes, err := client.Nodes(r.Context())
	if err != nil {
		_ = s.st.UpdatePVEHostStatus(r.Context(), host.ID, "error", host.LastSeen)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "nodes": 0, "error": err.Error()})
		return
	}
	now := store.Now()
	if err := s.st.UpdatePVEHostStatus(r.Context(), host.ID, "online", &now); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nodes": len(nodes)})
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeletePVEHost(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) refreshHostVMs(r *http.Request, host *store.PVEHost) ([]store.VM, error) {
	client, err := sched.PVEClient(host)
	if err != nil {
		return nil, err
	}
	vms, err := client.AllVMs(r.Context())
	if err != nil {
		return nil, err
	}
	out := make([]store.VM, 0, len(vms))
	for _, v := range vms {
		out = append(out, store.VM{
			VMID: v.VMID, Name: v.Name, Node: v.Node, Status: v.Status,
			MaxDisk: v.MaxDisk, MaxMem: v.MaxMem, Uptime: v.Uptime,
			HostID: host.ID, HostName: host.Name,
		})
	}
	if err := s.st.ReplaceVMCache(r.Context(), host.ID, out); err != nil {
		return nil, err
	}
	now := store.Now()
	if err := s.st.UpdatePVEHostStatus(r.Context(), host.ID, "online", &now); err != nil {
		return nil, err
	}
	return out, nil
}

// vmDTO matches the inventory shape in the API contract.
type vmDTO struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	MaxDisk  int64  `json:"maxdisk"`
	MaxMem   int64  `json:"maxmem"`
	Uptime   int64  `json:"uptime"`
	HostID   string `json:"hostId,omitempty"`
	HostName string `json:"hostName,omitempty"`
}

func toVMDTO(v store.VM, withHost bool) vmDTO {
	d := vmDTO{
		VMID: v.VMID, Name: v.Name, Node: v.Node, Status: v.Status,
		MaxDisk: v.MaxDisk, MaxMem: v.MaxMem, Uptime: v.Uptime,
	}
	if withHost {
		d.HostID = v.HostID
		d.HostName = v.HostName
	}
	return d
}

func (s *Server) handleHostVMs(w http.ResponseWriter, r *http.Request) {
	host, err := s.st.PVEHostByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	vms, err := s.refreshHostVMs(r, host)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not query Proxmox host: "+err.Error())
		return
	}
	out := make([]vmDTO, 0, len(vms))
	for _, v := range vms {
		out = append(out, toVMDTO(v, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.st.ListCachedVMs(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]vmDTO, 0, len(vms))
	for _, v := range vms {
		out = append(out, toVMDTO(v, true))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- targets

type targetDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Region    string `json:"region"`
	PathStyle bool   `json:"pathStyle"`
	Status    string `json:"status"`
}

func toTargetDTO(t *store.S3Target) targetDTO {
	return targetDTO{
		ID: t.ID, Name: t.Name, Endpoint: t.Endpoint, Bucket: t.Bucket,
		Region: t.Region, PathStyle: t.PathStyle, Status: t.Status,
	}
}

func (s *Server) handleListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.st.ListS3Targets(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]targetDTO, 0, len(targets))
	for _, t := range targets {
		out = append(out, toTargetDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

type createTargetRequest struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	PathStyle bool   `json:"pathStyle"`
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var body createTargetRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Bucket = strings.TrimSpace(body.Bucket)
	if body.Name == "" || body.Bucket == "" {
		writeError(w, http.StatusBadRequest, "name and bucket are required")
		return
	}
	if body.Region == "" {
		body.Region = "us-east-1"
	}
	target, err := s.st.CreateS3Target(r.Context(), &store.S3Target{
		Name: body.Name, Endpoint: strings.TrimRight(strings.TrimSpace(body.Endpoint), "/"),
		Region: body.Region, Bucket: body.Bucket,
		AccessKey: body.AccessKey, SecretKey: body.SecretKey, PathStyle: body.PathStyle,
		Status: "unknown",
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Best effort connectivity probe so the UI shows a status immediately.
	status := "online"
	if err := s.probeTarget(r, target); err != nil {
		s.log.Warn("target probe failed", "target", target.Name, "error", err)
		status = "error"
	}
	if err := s.st.UpdateS3TargetStatus(r.Context(), target.ID, status); err != nil {
		s.serverError(w, err)
		return
	}
	target.Status = status
	s.log.Info("backup target added", "target", target.Name, "bucket", target.Bucket, "status", status)
	writeJSON(w, http.StatusOK, toTargetDTO(target))
}

func (s *Server) probeTarget(r *http.Request, t *store.S3Target) error {
	client, err := s3target.New(r.Context(), s3target.Config{
		Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket,
		AccessKey: t.AccessKey, SecretKey: t.SecretKey, PathStyle: t.PathStyle,
	})
	if err != nil {
		return err
	}
	return client.Test(r.Context())
}

func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	target, err := s.st.S3TargetByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	if err := s.probeTarget(r, target); err != nil {
		_ = s.st.UpdateS3TargetStatus(r.Context(), target.ID, "error")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := s.st.UpdateS3TargetStatus(r.Context(), target.ID, "online"); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteS3Target(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
