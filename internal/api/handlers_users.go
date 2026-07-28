package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"proxback/internal/auth"
	"proxback/internal/store"
)

// User administration. Every route here is admin only, which the group it is
// registered in enforces — see apiRoutes. A user changing their *own* password
// does not come through here: that is POST /api/me/password, which needs no role
// because it is their own account.
//
// A password hash never leaves the store: every response is a userDTO, which has
// no field for one.

// userIDParam reads the {id} path parameter.
func userIDParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, out)
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var body createUserRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	if len(body.Password) < auth.MinPasswordLength {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	role, ok := store.ParseRole(strings.TrimSpace(body.Role))
	if !ok {
		writeError(w, http.StatusBadRequest, `role must be "admin", "operator" or "viewer"`)
		return
	}
	user, err := s.auth.CreateUserWithRole(r.Context(), username, body.Password, role)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateUser) {
			writeError(w, http.StatusConflict, "a user called "+username+" already exists")
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditUserCreate, ObjectKind: "user",
		ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
		Detail: "role " + string(user.Role),
	})
	s.log.Info("user created", "user", user.Username, "role", user.Role)
	writeJSON(w, http.StatusOK, toUserDTO(user))
}

type patchUserRequest struct {
	Role     *string `json:"role"`
	Password *string `json:"password"`
}

// handlePatchUser changes a user's role, their password, or both. Demoting the
// last admin is refused with 409: an installation whose only admin becomes an
// operator can never be administered again.
func (s *Server) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	id, err := userIDParam(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	user, err := s.st.UserByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "user")
		return
	}
	var body patchUserRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Role == nil && body.Password == nil {
		writeError(w, http.StatusBadRequest, "nothing to change: send role, password or both")
		return
	}
	var role store.Role
	if body.Role != nil {
		parsed, ok := store.ParseRole(strings.TrimSpace(*body.Role))
		if !ok {
			writeError(w, http.StatusBadRequest, `role must be "admin", "operator" or "viewer"`)
			return
		}
		role = parsed
	}
	if body.Password != nil && len(*body.Password) < auth.MinPasswordLength {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	// The role change goes first: it is the one that can be refused, and refusing
	// it after a password has already been replaced would be a half-applied edit.
	var changed []string
	if body.Role != nil && role != user.Role {
		if err := s.st.UpdateUserRole(r.Context(), user.ID, role); err != nil {
			if errors.Is(err, store.ErrLastAdmin) {
				s.audit(r, store.AuditEntry{
					Action: store.AuditUserModify, Result: store.AuditError, ObjectKind: "user",
					ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
					Detail: "refused: cannot demote the last admin",
				})
				writeError(w, http.StatusConflict,
					"this is the last admin: demoting it would leave nobody able to administer ProxBack")
				return
			}
			s.notFoundOr(w, err, "user")
			return
		}
		changed = append(changed, "role "+string(user.Role)+" to "+string(role))
	}
	if body.Password != nil {
		if err := s.auth.SetPassword(r.Context(), user.ID, *body.Password); err != nil {
			s.notFoundOr(w, err, "user")
			return
		}
		changed = append(changed, "password")
		// An admin resetting somebody else's password revokes that account's live
		// sessions — a reset exists to lock somebody out. The acting admin's own
		// session survives resetting their own password.
		if err := s.st.DeleteOtherSessions(r.Context(), user.ID, auth.SessionTokenFromRequest(r)); err != nil {
			s.log.Warn("could not revoke sessions after a password reset", "user", user.Username, "error", err)
		}
	}
	updated, err := s.st.UserByID(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(changed) > 0 {
		// Only what changed is recorded, never the new password.
		s.audit(r, store.AuditEntry{
			Action: store.AuditUserModify, ObjectKind: "user",
			ObjectID: strconv.FormatInt(updated.ID, 10), ObjectName: updated.Username,
			Detail: "changed " + strings.Join(changed, ", "),
		})
		s.log.Info("user modified", "user", updated.Username, "changed", strings.Join(changed, ", "))
	}
	writeJSON(w, http.StatusOK, toUserDTO(updated))
}

// handleDeleteUser removes a user. Deleting the last admin is refused with 409,
// and deleting anybody else revokes their sessions immediately, so a deleted
// account cannot keep browsing on a cookie it already holds.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := userIDParam(r)
	if err != nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	user, err := s.st.UserByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "user")
		return
	}
	if err := s.st.DeleteUser(r.Context(), user.ID); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			s.audit(r, store.AuditEntry{
				Action: store.AuditUserDelete, Result: store.AuditError, ObjectKind: "user",
				ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
				Detail: "refused: cannot delete the last admin",
			})
			writeError(w, http.StatusConflict,
				"this is the last admin: deleting it would leave nobody able to administer ProxBack")
			return
		}
		s.notFoundOr(w, err, "user")
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditUserDelete, ObjectKind: "user",
		ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
		Detail: "role " + string(user.Role) + ", sessions revoked",
	})
	s.log.Info("user deleted", "user", user.Username, "role", user.Role)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
