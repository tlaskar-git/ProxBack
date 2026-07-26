// Package auth implements password hashing, opaque session tokens and agent API keys.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"proxback/internal/store"
)

// SessionCookieName is the name of the browser session cookie.
const SessionCookieName = "proxback_session"

// SessionTTL is how long a session stays valid.
const SessionTTL = 30 * 24 * time.Hour

// EnrollTokenTTL is the lifetime of an agent enrollment token.
const EnrollTokenTTL = 24 * time.Hour

// ErrInvalidCredentials is returned when a login attempt fails.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Service provides authentication operations backed by the store.
type Service struct {
	st *store.Store
}

// New creates an auth service.
func New(st *store.Store) *Service { return &Service{st: st} }

// HashPassword bcrypt-hashes a plaintext password.
func HashPassword(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword verifies a plaintext password against a bcrypt hash.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// randomHex returns n cryptographically random bytes, hex encoded.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// NeedsSetup reports whether no user exists yet.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// CreateUser creates a user with a bcrypt-hashed password.
func (s *Service) CreateUser(ctx context.Context, username, password string) (*store.User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	return s.st.CreateUser(ctx, username, hash)
}

// DefaultAdminUsername and DefaultAdminPassword are the credentials seeded on
// first run. The UI nags until the password is changed.
const (
	DefaultAdminUsername = "admin"
	DefaultAdminPassword = "admin"
)

// SeedDefaultAdmin creates the default admin/admin account when no user exists
// yet and marks the installation as running on default credentials. It reports
// whether it created the account.
func (s *Service) SeedDefaultAdmin(ctx context.Context) (bool, error) {
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if _, err := s.CreateUser(ctx, DefaultAdminUsername, DefaultAdminPassword); err != nil {
		return false, err
	}
	if err := s.st.SetDefaultPasswordFlag(ctx, true); err != nil {
		return false, err
	}
	return true, nil
}

// ChangePassword verifies the current password and replaces it. The default-
// credentials flag is cleared and every other session of the user is revoked.
func (s *Service) ChangePassword(ctx context.Context, userID int64, current, newPassword, keepToken string) error {
	u, err := s.st.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, current) {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.st.UpdateUserPassword(ctx, userID, hash); err != nil {
		return err
	}
	if err := s.st.SetDefaultPasswordFlag(ctx, false); err != nil {
		return err
	}
	return s.st.DeleteOtherSessions(ctx, userID, keepToken)
}

// Login verifies credentials and returns the user.
func (s *Service) Login(ctx context.Context, username, password string) (*store.User, error) {
	u, err := s.st.UserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Spend comparable time to avoid trivially distinguishing unknown users.
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinvalidinva"), []byte(password))
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// StartSession creates a session and returns the opaque token.
func (s *Service) StartSession(ctx context.Context, userID int64) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if _, err := s.st.CreateSession(ctx, token, userID, SessionTTL); err != nil {
		return "", err
	}
	return token, nil
}

// UserForSession resolves a session token to its user.
func (s *Service) UserForSession(ctx context.Context, token string) (*store.User, error) {
	sess, err := s.st.SessionByToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return s.st.UserByID(ctx, sess.UserID)
}

// EndSession invalidates a session token.
func (s *Service) EndSession(ctx context.Context, token string) error {
	return s.st.DeleteSession(ctx, token)
}

// SetSessionCookie writes the session cookie on a response.
func SetSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// SessionTokenFromRequest extracts the session token from the request cookie.
func SessionTokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// ---------------------------------------------------------------- agent keys

// NewAgentKey generates a random 32 byte (64 hex character) agent API key.
func NewAgentKey() (string, error) { return randomHex(32) }

// HashAgentKey returns the storage form of an agent API key.
func HashAgentKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// SameKeyHash compares two key hashes in constant time.
func SameKeyHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// NewEnrollToken generates a random enrollment token.
func NewEnrollToken() (string, error) { return randomHex(24) }

// BearerToken extracts a bearer token from the Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return ""
	}
	if !equalFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
