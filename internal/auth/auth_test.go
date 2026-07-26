package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"proxback/internal/auth"
	"proxback/internal/store"
)

func newService(t *testing.T) (*auth.Service, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return auth.New(st), st
}

func TestSessionFlow(t *testing.T) {
	ctx := context.Background()
	svc, st := newService(t)

	needs, err := svc.NeedsSetup(ctx)
	if err != nil || !needs {
		t.Fatalf("NeedsSetup on empty store = %v, %v", needs, err)
	}
	user, err := svc.CreateUser(ctx, "admin", "correct horse battery")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.PasswordHash == "correct horse battery" || user.PasswordHash == "" {
		t.Fatalf("password does not look hashed: %q", user.PasswordHash)
	}
	if needs, err = svc.NeedsSetup(ctx); err != nil || needs {
		t.Fatalf("NeedsSetup after setup = %v, %v", needs, err)
	}

	if _, err := svc.Login(ctx, "admin", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.Login(ctx, "nobody", "correct horse battery"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("unknown user error = %v, want ErrInvalidCredentials", err)
	}
	logged, err := svc.Login(ctx, "admin", "correct horse battery")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if logged.ID != user.ID {
		t.Fatalf("login returned user %d, want %d", logged.ID, user.ID)
	}

	token, err := svc.StartSession(ctx, user.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(token))
	}
	resolved, err := svc.UserForSession(ctx, token)
	if err != nil || resolved.ID != user.ID {
		t.Fatalf("UserForSession = %v, %v", resolved, err)
	}
	if _, err := svc.UserForSession(ctx, "not-a-token"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token error = %v, want ErrNotFound", err)
	}
	if err := svc.EndSession(ctx, token); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if _, err := svc.UserForSession(ctx, token); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session survived logout: %v", err)
	}

	// A second session for the same user is independent.
	t1, _ := svc.StartSession(ctx, user.ID)
	t2, _ := svc.StartSession(ctx, user.ID)
	if t1 == t2 {
		t.Fatal("two sessions share a token")
	}
	if err := st.PurgeExpiredSessions(ctx); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := svc.UserForSession(ctx, t1); err != nil {
		t.Fatalf("purge removed a live session: %v", err)
	}
}

func TestSessionCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	auth.SetSessionCookie(rec, "tok123")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	c := cookies[0]
	if c.Name != auth.SessionCookieName {
		t.Fatalf("cookie name = %q, want %q", c.Name, auth.SessionCookieName)
	}
	if c.Value != "tok123" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie attributes wrong: %+v", c)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	if got := auth.SessionTokenFromRequest(req); got != "" {
		t.Fatalf("token from bare request = %q", got)
	}
	req.AddCookie(c)
	if got := auth.SessionTokenFromRequest(req); got != "tok123" {
		t.Fatalf("token from request = %q", got)
	}

	rec = httptest.NewRecorder()
	auth.ClearSessionCookie(rec)
	cleared := rec.Result().Cookies()[0]
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("cleared cookie = %+v", cleared)
	}
}

func TestAgentKeys(t *testing.T) {
	key, err := auth.NewAgentKey()
	if err != nil {
		t.Fatalf("new agent key: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("agent key length = %d, want 64 hex chars (32 bytes)", len(key))
	}
	other, _ := auth.NewAgentKey()
	if other == key {
		t.Fatal("agent keys are not random")
	}
	hash := auth.HashAgentKey(key)
	if hash == key {
		t.Fatal("agent key stored in the clear")
	}
	if !auth.SameKeyHash(hash, auth.HashAgentKey(key)) {
		t.Fatal("hashing is not stable")
	}
	if auth.SameKeyHash(hash, auth.HashAgentKey(other)) {
		t.Fatal("different keys hash the same")
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER abc":  "abc",
		"Basic abc":   "",
		"":            "",
		"Bearer":      "",
		"Bearer  abc": " abc",
	}
	for header, want := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/agents/heartbeat", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if got := auth.BearerToken(req); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}
