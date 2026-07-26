package api

import (
	"errors"
	"net/http"
	"strings"

	"proxback/internal/auth"
	"proxback/internal/store"
	"proxback/internal/version"
)

type userDTO struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func toUserDTO(u *store.User) userDTO {
	return userDTO{ID: u.ID, Username: u.Username}
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
	if len(body.Password) < 8 {
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
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(user)})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}
	user, err := s.auth.Login(r.Context(), strings.TrimSpace(body.Username), body.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
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
	auth.SetSessionCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(user)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.SessionTokenFromRequest(r); token != "" {
		if err := s.auth.EndSession(r.Context(), token); err != nil {
			s.log.Warn("could not delete session", "error", err)
		}
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
	if len(body.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	err := s.auth.ChangePassword(r.Context(), user.ID, body.CurrentPassword, body.NewPassword,
		auth.SessionTokenFromRequest(r))
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeError(w, http.StatusUnauthorized, "current password is incorrect")
			return
		}
		s.serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
