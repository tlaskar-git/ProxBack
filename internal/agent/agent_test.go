package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestBackoffFor(t *testing.T) {
	t.Parallel()
	base := time.Second
	cases := []struct {
		fails int
		want  time.Duration
	}{
		{-1, base},
		{0, base},
		{1, base},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{5, 16 * time.Second},
		{20, MaxRetryInterval},
		{1000, MaxRetryInterval},
	}
	for _, tc := range cases {
		if got := backoffFor(base, tc.fails); got != tc.want {
			t.Fatalf("backoffFor(%s, %d) = %s, want %s", base, tc.fails, got, tc.want)
		}
	}
	if got := backoffFor(0, 1); got != DefaultHeartbeatInterval {
		t.Fatalf("backoffFor(0, 1) = %s, want the default interval", got)
	}
	// Backoff must never exceed the cap, whatever the base.
	if got := backoffFor(time.Hour, 3); got != MaxRetryInterval {
		t.Fatalf("backoffFor(1h, 3) = %s, want the cap %s", got, MaxRetryInterval)
	}
}

// enrollingServer answers registration and counts heartbeats, replying to them
// with the supplied status code.
func enrollingServer(t *testing.T, heartbeatStatus int, beats *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"agentId": "agent-1", "apiKey": "key-1"})
	})
	mux.HandleFunc("/api/agents/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		beats.Add(1)
		if heartbeatStatus == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jobs":[]}`))
			return
		}
		http.Error(w, "server is having a bad day", heartbeatStatus)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunKeepsRetryingWhenHeartbeatsFail(t *testing.T) {
	t.Parallel()
	var beats atomic.Int64
	srv := enrollingServer(t, http.StatusInternalServerError, &beats)

	a, err := New(Config{
		ServerURL:         srv.URL,
		Token:             "tok",
		ConfigDir:         t.TempDir(),
		HeartbeatInterval: 5 * time.Millisecond,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	// The whole point: a server that answers every heartbeat with an error must
	// not take the agent down. Under a service manager that would look like a
	// service that starts and immediately stops.
	deadline := time.Now().Add(5 * time.Second)
	for beats.Load() < 4 {
		select {
		case err := <-done:
			t.Fatalf("Run returned early after %d failed heartbeats: %v", beats.Load(), err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d heartbeats in 5s, want at least 4", beats.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestRunKeepsRetryingWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()
	// A server URL that nothing is listening on: enrollment cannot complete,
	// but the agent must wait for the server to come back rather than exit.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	a, err := New(Config{
		ServerURL:         url,
		Token:             "tok",
		ConfigDir:         t.TempDir(),
		HeartbeatInterval: 5 * time.Millisecond,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned while the server was unreachable: %v", err)
	case <-time.After(120 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestRunFailsFastOnARejectedToken(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agents/register", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid or expired enrollment token", http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a, err := New(Config{
		ServerURL:         srv.URL,
		Token:             "spent",
		ConfigDir:         t.TempDir(),
		HeartbeatInterval: 5 * time.Millisecond,
		Logger:            testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrServerUnreachable) {
			t.Fatalf("Run = %v, want a non-retryable registration error", err)
		}
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("Run error %q does not mention the HTTP status", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail fast on a rejected token")
	}
}

func TestEnrollExplainsAnUnreachableServer(t *testing.T) {
	t.Parallel()
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()

	a, err := New(Config{
		ServerURL: url,
		Token:     "tok",
		ConfigDir: t.TempDir(),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = a.Enroll(context.Background())
	if err == nil {
		t.Fatal("Enroll = nil, want an error")
	}
	if !errors.Is(err, ErrServerUnreachable) {
		t.Fatalf("Enroll error %v is not classified as unreachable", err)
	}
	for _, want := range []string{"cannot reach the ProxBack server", "--insecure"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Enroll error %q does not mention %q", err, want)
		}
	}
}

func TestEnrollCreatesTheConfigDirectory(t *testing.T) {
	t.Parallel()
	var beats atomic.Int64
	srv := enrollingServer(t, http.StatusOK, &beats)

	// A standalone guest has no /etc/proxback or %ProgramData%\ProxBack yet.
	dir := filepath.Join(t.TempDir(), "missing", "ProxBack")
	a, err := New(Config{
		ServerURL: srv.URL,
		Token:     "tok",
		ConfigDir: dir,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Enroll(context.Background()); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if a.AgentID() != "agent-1" {
		t.Fatalf("AgentID = %q, want agent-1", a.AgentID())
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); err != nil {
		t.Fatalf("stat %s: %v", ConfigFileName, err)
	}

	// A second agent over the same directory reuses the stored key and never
	// talks to the server again.
	b, err := New(Config{ConfigDir: dir, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New (second): %v", err)
	}
	if err := b.Enroll(context.Background()); err != nil {
		t.Fatalf("Enroll (second): %v", err)
	}
	if b.AgentID() != "agent-1" {
		t.Fatalf("second AgentID = %q, want agent-1", b.AgentID())
	}
}

func TestEnrollRequiresAServerURL(t *testing.T) {
	t.Parallel()
	a, err := New(Config{ConfigDir: t.TempDir(), Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = a.Enroll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--server is required") {
		t.Fatalf("Enroll = %v, want the missing --server error", err)
	}
	if errors.Is(err, ErrServerUnreachable) {
		t.Fatal("a missing --server must not be treated as a retryable transport failure")
	}
}

func TestRunOnceSucceedsAgainstAHealthyServer(t *testing.T) {
	t.Parallel()
	var beats atomic.Int64
	srv := enrollingServer(t, http.StatusOK, &beats)

	a, err := New(Config{
		ServerURL: srv.URL,
		Token:     "tok",
		ConfigDir: t.TempDir(),
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if beats.Load() != 1 {
		t.Fatalf("heartbeats = %d, want 1", beats.Load())
	}
}

func TestSleepCtxHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx = %v, want context.Canceled", err)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleepCtx = %v, want nil", err)
	}
}
