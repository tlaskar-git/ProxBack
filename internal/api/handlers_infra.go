package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"proxback/internal/blobstore"
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
	if vms, err := s.refreshHostVMs(r, host); err != nil {
		s.log.Warn("could not refresh vm inventory", "host", host.Name, "error", err)
	} else if len(vms) == 0 {
		if warning := client.DiagnoseEmptyInventory(r.Context()); warning != "" {
			_ = s.st.UpdatePVEHostStatus(r.Context(), host.ID, "limited", &now)
			host.Status = "limited"
			s.log.Warn("proxmox token has no privileges", "host", host.Name, "hint", warning)
		}
	}
	// The endpoint and token id are recorded; the token secret never is.
	s.audit(r, store.AuditEntry{
		Action: store.AuditHostCreate, ObjectKind: "host",
		ObjectID: host.ID, ObjectName: host.Name,
		Detail: host.BaseURL + " as " + host.TokenID,
	})
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
	// Connectivity is fine — but a token with no privileges sees an empty
	// cluster, which reads as "connected" while every listing is silently
	// empty. Surface that here, where the operator is looking.
	warning := ""
	status := "online"
	if vms, verr := client.AllVMs(r.Context()); verr == nil && len(vms) == 0 {
		if warning = client.DiagnoseEmptyInventory(r.Context()); warning != "" {
			status = "limited"
		}
	}
	now := store.Now()
	if err := s.st.UpdatePVEHostStatus(r.Context(), host.ID, status, &now); err != nil {
		s.serverError(w, err)
		return
	}
	out := map[string]any{"ok": true, "nodes": len(nodes)}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := ""
	if host, err := s.st.PVEHostByID(r.Context(), id); err == nil {
		name = host.Name
	}
	if err := s.st.DeletePVEHost(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditHostDelete, ObjectKind: "host", ObjectID: id, ObjectName: name,
	})
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
			Tags: v.Tags, HostID: host.ID, HostName: host.Name,
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
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
	// Tags is always an array, never null, so the UI can map over it directly.
	Tags     []string `json:"tags"`
	MaxDisk  int64    `json:"maxdisk"`
	MaxMem   int64    `json:"maxmem"`
	Uptime   int64    `json:"uptime"`
	HostID   string   `json:"hostId,omitempty"`
	HostName string   `json:"hostName,omitempty"`
}

func toVMDTO(v store.VM, withHost bool) vmDTO {
	d := vmDTO{
		VMID: v.VMID, Name: v.Name, Node: v.Node, Status: v.Status,
		Tags:    v.Tags,
		MaxDisk: v.MaxDisk, MaxMem: v.MaxMem, Uptime: v.Uptime,
	}
	if d.Tags == nil {
		d.Tags = []string{}
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
	if len(vms) == 0 {
		if client, cerr := sched.PVEClient(host); cerr == nil {
			if warning := client.DiagnoseEmptyInventory(r.Context()); warning != "" {
				now := store.Now()
				_ = s.st.UpdatePVEHostStatus(r.Context(), host.ID, "limited", &now)
				writeError(w, http.StatusConflict, "host "+host.Name+": "+warning)
				return
			}
		}
	}
	out := make([]vmDTO, 0, len(vms))
	for _, v := range vms {
		out = append(out, toVMDTO(v, false))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFreeVMID suggests the lowest guest id a restore can safely land on:
// the first id at or above 100 (or an optional ?after= hint) that no guest on
// the host occupies. It is what makes "alongside" the easy choice rather than
// the annoying one.
func (s *Server) handleFreeVMID(w http.ResponseWriter, r *http.Request) {
	host, err := s.st.PVEHostByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "host")
		return
	}
	after := 0
	if v := strings.TrimSpace(r.URL.Query().Get("after")); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	vmid, err := s.sched.FreeVMIDForHost(r.Context(), host.ID, after)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not query Proxmox host: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"vmid": vmid})
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

// targetDTO is the wire shape of a backup target. Which half is populated
// follows kind: an S3 target carries endpoint/bucket/region/pathStyle, a
// filesystem target carries path plus its capacity. freeBytes and totalBytes are
// 0 when they do not apply or the platform cannot report them.
type targetDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	Endpoint   string `json:"endpoint"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	PathStyle  bool   `json:"pathStyle"`
	Status     string `json:"status"`
	FreeBytes  int64  `json:"freeBytes"`
	TotalBytes int64  `json:"totalBytes"`
	// Warnings carries the diagnostics of a connection test (a path that is not a
	// mount point, a target sharing a disk with ProxBack itself). It is only
	// present on the responses that ran one.
	Warnings []blobstore.Warning `json:"warnings,omitempty"`
}

func toTargetDTO(t *store.S3Target) targetDTO {
	d := targetDTO{
		ID: t.ID, Name: t.Name, Kind: t.Kind, Path: t.Path,
		Endpoint: t.Endpoint, Bucket: t.Bucket,
		Region: t.Region, PathStyle: t.PathStyle, Status: t.Status,
	}
	if t.IsFilesystem() {
		// Capacity is read live: it is the number that tells an operator a target is
		// about to fill up, and a cached one would be worse than none.
		d.FreeBytes, d.TotalBytes = blobstore.Capacity(t.Path)
	}
	return d
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
	Name string `json:"name"`
	// Kind is "s3" or "filesystem". Empty is inferred from the fields present, so
	// a client that predates filesystem targets keeps working unchanged.
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	PathStyle bool   `json:"pathStyle"`
	// AllowSameFilesystem accepts a filesystem target on the same filesystem as
	// ProxBack's own data directory. It is refused by default because a backup on
	// the disk it is protecting dies with that disk; the single-disk homelab that
	// really means it says so here.
	AllowSameFilesystem bool `json:"allowSameFilesystem"`
}

// normalize trims the request, settles its kind and rejects a mix of the two
// shapes. Sending S3 credentials with a path is not a target ProxBack can make
// sense of, and quietly ignoring half of the request is how an operator ends up
// backing up somewhere they did not intend.
func (b *createTargetRequest) normalize() error {
	b.Name = strings.TrimSpace(b.Name)
	b.Kind = strings.ToLower(strings.TrimSpace(b.Kind))
	b.Path = strings.TrimSpace(b.Path)
	b.Bucket = strings.TrimSpace(b.Bucket)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.Region = strings.TrimSpace(b.Region)
	if b.Name == "" {
		return errors.New("name is required")
	}
	if b.Kind == "" {
		if b.Path != "" {
			b.Kind = store.TargetKindFilesystem
		} else {
			b.Kind = store.TargetKindS3
		}
	}
	switch b.Kind {
	case store.TargetKindFilesystem:
		if b.Path == "" {
			return errors.New(`a filesystem target requires "path": the directory or mount point to back up to`)
		}
		var s3Fields []string
		for _, f := range []struct {
			name string
			set  bool
		}{
			{"endpoint", b.Endpoint != ""},
			{"region", b.Region != ""},
			{"bucket", b.Bucket != ""},
			{"accessKey", b.AccessKey != ""},
			{"secretKey", b.SecretKey != ""},
			{"pathStyle", b.PathStyle},
		} {
			if f.set {
				s3Fields = append(s3Fields, f.name)
			}
		}
		if len(s3Fields) > 0 {
			return fmt.Errorf(`a filesystem target takes only "path", but object storage fields were also set: %s`,
				strings.Join(s3Fields, ", "))
		}
		return nil
	case store.TargetKindS3:
		if b.Bucket == "" {
			return errors.New(`an S3 target requires "bucket"`)
		}
		if b.Path != "" {
			return errors.New(`an S3 target has no "path" — remove it, or set "kind":"filesystem" to back up to a directory`)
		}
		if b.AllowSameFilesystem {
			return errors.New(`"allowSameFilesystem" only applies to a filesystem target`)
		}
		if b.Region == "" {
			b.Region = "us-east-1"
		}
		return nil
	default:
		return fmt.Errorf("unknown target kind %q: use \"s3\" or \"filesystem\"", b.Kind)
	}
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	var body createTargetRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := body.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	target := &store.S3Target{
		Name: body.Name, Kind: body.Kind, Status: "unknown",
	}
	var warnings []blobstore.Warning
	if body.Kind == store.TargetKindFilesystem {
		// A filesystem target is checked *before* it is stored: an unwritable path or
		// one on ProxBack's own disk is a configuration mistake to report, not a
		// broken target to save.
		diag, err := blobstore.Check(blobstore.CheckRequest{
			Path:                body.Path,
			DataDir:             s.dataDir,
			AllowSameFilesystem: body.AllowSameFilesystem,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Store the resolved absolute path: a relative one would depend on the
		// server's working directory, which nothing should.
		target.Path = diag.Path
		warnings = diag.Warnings
		for _, warn := range warnings {
			s.log.Warn("filesystem target warning", "target", body.Name, "code", warn.Code, "detail", warn.Detail)
		}
	} else {
		target.Endpoint = s3target.NormalizeEndpoint(body.Endpoint)
		target.Region = body.Region
		target.Bucket = body.Bucket
		target.AccessKey = body.AccessKey
		target.SecretKey = body.SecretKey
		target.PathStyle = body.PathStyle
	}
	target, err := s.st.CreateS3Target(r.Context(), target)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Best effort connectivity probe so the UI shows a status immediately.
	status := "online"
	if _, err := s.probeTarget(r, target); err != nil {
		s.log.Warn("target probe failed", "target", target.Name, "error", err)
		status = "error"
	}
	if err := s.st.UpdateS3TargetStatus(r.Context(), target.ID, status); err != nil {
		s.serverError(w, err)
		return
	}
	target.Status = status
	s.log.Info("backup target added", "target", target.Name, "kind", target.Kind,
		"bucket", target.Bucket, "path", target.Path, "status", status)
	// Where the target points is recorded; the access key and secret key are not.
	s.audit(r, store.AuditEntry{
		Action: store.AuditTargetCreate, ObjectKind: "target",
		ObjectID: target.ID, ObjectName: target.Name,
		Detail: targetAuditDetail(target),
	})
	out := toTargetDTO(target)
	out.Warnings = warnings
	writeJSON(w, http.StatusOK, out)
}

// probeTarget runs the target's own connection test: a probe object round trip on
// object storage, the full path diagnosis on a filesystem target. The diagnosis is
// nil for an S3 target, which has no path to diagnose.
//
// Testing an *existing* filesystem target never refuses it for sharing a
// filesystem with the data directory — that decision was made when the target was
// created, and flipping a working target to "error" for it would be a lie. The
// warning is still reported.
func (s *Server) probeTarget(r *http.Request, t *store.S3Target) (*blobstore.Diagnosis, error) {
	if t.IsFilesystem() {
		diag, err := blobstore.Check(blobstore.CheckRequest{
			Path:                t.Path,
			DataDir:             s.dataDir,
			AllowSameFilesystem: true,
		})
		return &diag, err
	}
	bs, err := sched.StoreForTarget(r.Context(), t)
	if err != nil {
		return nil, err
	}
	return nil, bs.Test(r.Context())
}

func (s *Server) handleTestTarget(w http.ResponseWriter, r *http.Request) {
	target, err := s.st.S3TargetByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	diag, probeErr := s.probeTarget(r, target)
	out := map[string]any{"ok": probeErr == nil}
	if probeErr != nil {
		out["error"] = probeErr.Error()
	}
	if diag != nil {
		// Structured diagnostics, not a pass/fail: "it works, but that path is not a
		// mount point" is the single most useful thing to tell a NAS operator.
		out["path"] = diag.Path
		out["freeBytes"] = diag.FreeBytes
		out["totalBytes"] = diag.TotalBytes
		out["isMountPoint"] = diag.IsMountPoint
		out["sameFilesystemAsDataDir"] = diag.SameFilesystemAsDataDir
		if diag.FilesystemType != "" {
			out["filesystemType"] = diag.FilesystemType
		}
		if diag.MountPoint != "" {
			out["mountPoint"] = diag.MountPoint
		}
		out["warnings"] = diag.Warnings
	}
	status := "online"
	if probeErr != nil {
		status = "error"
	}
	if err := s.st.UpdateS3TargetStatus(r.Context(), target.ID, status); err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// targetAuditDetail describes where a target points, and nothing that could
// authenticate to it: never the access key, never the secret key.
func targetAuditDetail(t *store.S3Target) string {
	if t.IsFilesystem() {
		return "filesystem target at " + t.Path
	}
	return "s3 target " + t.Bucket + " at " + t.Endpoint
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name, detail := "", ""
	if target, err := s.st.S3TargetByID(r.Context(), id); err == nil {
		name = target.Name
		detail = targetAuditDetail(target)
	}
	if err := s.st.DeleteS3Target(r.Context(), id); err != nil {
		s.notFoundOr(w, err, "target")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditTargetDelete, ObjectKind: "target",
		ObjectID: id, ObjectName: name, Detail: detail,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
