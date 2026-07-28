package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"proxback/internal/browse"
	"proxback/internal/sched"
	"proxback/internal/store"
)

/*
Looking inside a restore point.

Listing and downloading are operator-level, not viewer-level. A file listing
names every path on a protected machine and a download hands back its contents,
which is the same disclosure a restore is — so it sits behind the same role, and
lands in the audit trail the same way.
*/

type browseEntryDTO struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Dir   bool   `json:"dir"`
	MTime string `json:"mtime,omitempty"`
	Link  string `json:"link,omitempty"`
}

type browseListResponse struct {
	Path    string           `json:"path"`
	Entries []browseEntryDTO `json:"entries"`
	// Truncated says the restore point held more entries than could be indexed,
	// so the listing is incomplete. Reported rather than hidden.
	Truncated bool `json:"truncated"`
}

func toDTO(entries []browse.Entry) []browseEntryDTO {
	out := make([]browseEntryDTO, 0, len(entries))
	for _, e := range entries {
		dto := browseEntryDTO{
			Name: path.Base(e.Path), Path: e.Path,
			Size: e.Size, Dir: e.Dir, Link: e.Link,
		}
		if !e.MTime.IsZero() {
			dto.MTime = e.MTime.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, dto)
	}
	return out
}

// handleBrowseBackup lists one directory of a restore point, or searches it.
func (s *Server) handleBrowseBackup(w http.ResponseWriter, r *http.Request) {
	backupID := chi.URLParam(r, "id")
	backup, err := s.st.BackupByID(r.Context(), backupID)
	if err != nil {
		s.browseFailure(w, err)
		return
	}

	if q := strings.TrimSpace(r.URL.Query().Get("search")); q != "" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		found, err := s.sched.BrowseSearch(r.Context(), backup, q, limit)
		if err != nil {
			s.browseFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, browseListResponse{Path: "", Entries: toDTO(found)})
		return
	}

	dir := r.URL.Query().Get("path")
	entries, truncated, err := s.sched.BrowseList(r.Context(), backup, dir)
	if err != nil {
		s.browseFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, browseListResponse{
		Path: strings.Trim(dir, "/"), Entries: toDTO(entries), Truncated: truncated,
	})
}

// handleDownloadBackupFile streams one file out of a restore point.
func (s *Server) handleDownloadBackupFile(w http.ResponseWriter, r *http.Request) {
	backupID := chi.URLParam(r, "id")
	filePath := r.URL.Query().Get("path")
	if strings.TrimSpace(filePath) == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	backup, err := s.st.BackupByID(r.Context(), backupID)
	if err != nil {
		s.browseFailure(w, err)
		return
	}
	entry, body, err := s.sched.BrowseOpenFile(r.Context(), backup, filePath)
	if err != nil {
		s.browseFailure(w, err)
		return
	}

	s.audit(r, store.AuditEntry{
		Action: store.AuditRestoreStart, ObjectKind: "backup",
		ObjectID: backupID, ObjectName: s.backupName(r, backupID),
		Detail: fmt.Sprintf("downloaded file %s (%d bytes) from this restore point", entry.Path, entry.Size),
	})

	name := path.Base(entry.Path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	// Downloaded content is guest data and must never be interpreted by the
	// browser: it is an attachment, sniffing is off, and the filename is quoted
	// so a name carrying quotes or newlines cannot forge extra headers.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	if _, err := io.Copy(w, body); err != nil {
		// The response is already committed, so this can only be logged.
		s.log.Warn("file download interrupted", "backup", backupID, "path", entry.Path, "error", err)
	}
}

// browseFailure maps a browse error to a status, keeping "this format cannot be
// browsed" apart from "something went wrong" — the first is an answer, not a
// fault, and the console shows it as guidance.
func (s *Server) browseFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "backup not found")
	case errors.Is(err, sched.ErrNoSuchEntry):
		writeError(w, http.StatusNotFound, "no such file in this restore point")
	case errors.Is(err, browse.ErrVMNotIndexable), errors.Is(err, browse.ErrNotBrowsable):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		s.serverError(w, err)
	}
}
