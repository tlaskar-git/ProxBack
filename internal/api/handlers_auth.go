package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"proxback/internal/auth"
	"proxback/internal/store"
	"proxback/internal/version"
)

// userDTO is how a user appears on the wire. There is deliberately no field for
// the password hash: it cannot leak from a shape that cannot carry it.
type userDTO struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	// Role is what this user may do, so the console can hide what they cannot.
	// Hiding is courtesy; the server enforces.
	Role      store.Role `json:"role"`
	CreatedAt time.Time  `json:"createdAt"`
	// LastLoginAt is absent for a user who has never signed in.
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
}

func toUserDTO(u *store.User) userDTO {
	return userDTO{
		ID: u.ID, Username: u.Username, Role: u.Role,
		CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt,
	}
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	needs, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	// defaultLogin tells the login page to hint at the seeded admin/admin
	// credentials until they are changed.
	def, err := s.st.DefaultPasswordFlag(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needsSetup": needs, "defaultLogin": def})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if len(body.Password) < auth.MinPasswordLength {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	needs, err := s.auth.NeedsSetup(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !needs {
		writeError(w, http.StatusConflict, "setup has already been completed")
		return
	}
	user, err := s.auth.CreateUser(r.Context(), body.Username, body.Password)
	if err != nil {
		s.serverError(w, err)
		return
	}
	token, err := s.auth.StartSession(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Setup creates the installation's first admin. It is recorded like any other
	// account creation, so the trail starts with who took ownership.
	s.audit(r, store.AuditEntry{
		Action: store.AuditUserCreate, Actor: user.Username, ActorID: user.ID,
		ObjectKind: "user", ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
		Detail: "first-run setup, role " + string(user.Role),
	})
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(user)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}
	username := strings.TrimSpace(body.Username)
	user, err := s.auth.Login(r.Context(), username, body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			// The attempted name is recorded — that is the point of a failed
			// sign-in entry — and the password never is.
			s.audit(r, store.AuditEntry{
				Action: store.AuditSignInFailed, Result: store.AuditDenied,
				Actor: username, ObjectKind: "user", ObjectName: username,
				Detail: "invalid username or password",
			})
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		s.serverError(w, err)
		return
	}
	token, err := s.auth.StartSession(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.st.TouchUserLogin(r.Context(), user.ID, store.Now()); err != nil {
		s.log.Warn("could not record the sign-in time", "user", user.Username, "error", err)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditSignIn, Actor: user.Username, ActorID: user.ID,
		ObjectKind: "user", ObjectName: user.Username,
		Detail: "role " + string(user.Role),
	})
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(user)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.SessionTokenFromRequest(r); token != "" {
		if err := s.auth.EndSession(r.Context(), token); err != nil {
			s.log.Warn("could not delete session", "error", err)
		}
	}
	if user := userFrom(r.Context()); user != nil {
		s.audit(r, store.AuditEntry{
			Action: store.AuditSignOut, ObjectKind: "user", ObjectName: user.Username,
		})
	}
	auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	def, err := s.st.DefaultPasswordFlag(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":               toUserDTO(user),
		"mustChangePassword": def,
		"serverVersion":      version.Version,
		// The role is on the user object and repeated at the top level, because
		// this is the one response every console build reads to decide what to
		// show; it is cheaper to answer both shapes than to guess.
		"role": user.Role,
	})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.NewPassword) < auth.MinPasswordLength {
		writeError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	err := s.auth.ChangePassword(r.Context(), user.ID, body.CurrentPassword, body.NewPassword,
		auth.SessionTokenFromRequest(r))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			s.audit(r, store.AuditEntry{
				Action: store.AuditUserModify, Result: store.AuditDenied, ObjectKind: "user",
				ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
				Detail: "own password change refused: current password is incorrect",
			})
			writeError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditUserModify, ObjectKind: "user",
		ObjectID: strconv.FormatInt(user.ID, 10), ObjectName: user.Username,
		Detail: "changed own password",
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
