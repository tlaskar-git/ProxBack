package helpermgr_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"proxback/internal/auth"
	"proxback/internal/helpermgr"
	"proxback/internal/store"
)

// newManager builds a manager over a store that already knows one Proxmox
// host, because a helper enrollment token is always minted for a host.
func newManager(t *testing.T) (*helpermgr.Manager, *store.Store, string) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	host, err := st.CreatePVEHost(context.Background(), &store.PVEHost{
		Name: "cluster-a", BaseURL: "https://pve-a.example:8006",
		TokenID: "root@pam!proxback", TokenSecret: "secret", Status: "online",
	})
	if err != nil {
		t.Fatalf("create host: %v", err)
	}
	return helpermgr.New(st, slog.New(slog.NewTextHandler(io.Discard, nil))), st, host.ID
}

func TestRegisterConsumesTheTokenAndLearnsTheAddress(t *testing.T) {
	ctx := context.Background()
	m, st, hostID := newManager(t)

	tok, err := m.CreateEnrollToken(ctx, hostID)
	if err != nil {
		t.Fatalf("create enroll token: %v", err)
	}
	if tok.Token == "" || !tok.ExpiresAt.After(time.Now()) {
		t.Fatalf("enroll token = %+v", tok)
	}
	if stored, err := st.EnrollTokenByValue(ctx, tok.Token); err != nil ||
		stored.Purpose != store.EnrollPurposeHelper {
		t.Fatalf("token purpose = %+v (%v)", stored, err)
	}

	res, err := m.Register(ctx, helpermgr.RegisterRequest{
		Token: tok.Token, Node: "pve2", Port: 8107, Version: "0.3.0",
		AccessSecret: "access-secret",
	}, "10.0.0.12:54321")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if res.HelperID == "" || len(res.APIKey) != 64 {
		t.Fatalf("registration = %+v", res)
	}

	h, err := st.HelperFor(ctx, hostID, "pve2")
	if err != nil {
		t.Fatalf("load registered helper: %v", err)
	}
	// The helper inherited the host from the token: the node never said which
	// cluster it belongs to.
	if h.HostID != hostID {
		t.Fatalf("registered helper hostId = %q, want %q", h.HostID, hostID)
	}
	// The address comes from the connection, and the port from the body.
	if h.Address != "10.0.0.12" || h.Port != 8107 {
		t.Fatalf("registered helper = %+v", h)
	}
	if h.AccessSecret != "access-secret" || h.Version != "0.3.0" {
		t.Fatalf("registered helper = %+v", h)
	}
	// The API key is only ever stored hashed.
	if h.APIKeyHash == res.APIKey || h.APIKeyHash != auth.HashAgentKey(res.APIKey) {
		t.Fatalf("api key hash = %q", h.APIKeyHash)
	}
	// Registration counts as a heartbeat, so a fresh helper shows up online.
	if !helpermgr.Online(h) || helpermgr.Status(h) != "online" {
		t.Fatalf("freshly registered helper is %s", helpermgr.Status(h))
	}

	// Authentication round trip.
	got, err := m.Authenticate(ctx, res.APIKey)
	if err != nil || got.ID != h.ID {
		t.Fatalf("authenticate = %+v (%v)", got, err)
	}
	for _, bad := range []string{"", "wrong-key"} {
		if _, err := m.Authenticate(ctx, bad); !errors.Is(err, helpermgr.ErrUnauthorized) {
			t.Fatalf("authenticate with %q = %v, want ErrUnauthorized", bad, err)
		}
	}

	// The token is spent.
	if _, err := m.Register(ctx, helpermgr.RegisterRequest{
		Token: tok.Token, Node: "pve3", AccessSecret: "s",
	}, "10.0.0.13:1"); !errors.Is(err, helpermgr.ErrBadToken) {
		t.Fatalf("reusing a token = %v, want ErrBadToken", err)
	}
}

func TestRegisterRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	m, _, hostID := newManager(t)

	if _, err := m.Register(ctx, helpermgr.RegisterRequest{Node: "pve2", AccessSecret: "s"}, "1.2.3.4:5"); !errors.Is(err, helpermgr.ErrBadToken) {
		t.Fatalf("register without a token = %v, want ErrBadToken", err)
	}
	if _, err := m.Register(ctx, helpermgr.RegisterRequest{Token: "nope", Node: "pve2", AccessSecret: "s"}, "1.2.3.4:5"); !errors.Is(err, helpermgr.ErrBadToken) {
		t.Fatalf("register with an unknown token = %v, want ErrBadToken", err)
	}

	tok, err := m.CreateEnrollToken(ctx, hostID)
	if err != nil {
		t.Fatalf("create enroll token: %v", err)
	}
	for _, c := range []struct {
		what string
		req  helpermgr.RegisterRequest
	}{
		{"no node", helpermgr.RegisterRequest{Token: tok.Token, AccessSecret: "s"}},
		{"blank node", helpermgr.RegisterRequest{Token: tok.Token, Node: "  ", AccessSecret: "s"}},
		{"no access secret", helpermgr.RegisterRequest{Token: tok.Token, Node: "pve2"}},
	} {
		if _, err := m.Register(ctx, c.req, "1.2.3.4:5"); err == nil {
			t.Fatalf("register with %s succeeded", c.what)
		}
	}
	// A rejected registration must not have spent the token.
	if _, err := m.Register(ctx, helpermgr.RegisterRequest{
		Token: tok.Token, Node: "pve2", AccessSecret: "s",
	}, "1.2.3.4:5"); err != nil {
		t.Fatalf("register after rejected attempts: %v", err)
	}
}

func TestRegisterDefaultsThePort(t *testing.T) {
	ctx := context.Background()
	m, st, hostID := newManager(t)
	tok, err := m.CreateEnrollToken(ctx, hostID)
	if err != nil {
		t.Fatalf("create enroll token: %v", err)
	}
	if _, err := m.Register(ctx, helpermgr.RegisterRequest{
		Token: tok.Token, Node: "pve2", AccessSecret: "s",
	}, "10.0.0.12:1"); err != nil {
		t.Fatalf("register: %v", err)
	}
	h, err := st.HelperFor(ctx, hostID, "pve2")
	if err != nil {
		t.Fatalf("load helper: %v", err)
	}
	if h.Port != store.DefaultHelperPort {
		t.Fatalf("port = %d, want %d", h.Port, store.DefaultHelperPort)
	}
}

// An enrollment token is minted for a host, and only for a host: without one
// the resulting registration could never be routed to.
func TestEnrollTokenRequiresAKnownHost(t *testing.T) {
	ctx := context.Background()
	m, _, hostID := newManager(t)

	for _, bad := range []string{"", "   ", "no-such-host"} {
		if _, err := m.CreateEnrollToken(ctx, bad); !errors.Is(err, helpermgr.ErrNoHost) {
			t.Fatalf("CreateEnrollToken(%q) = %v, want ErrNoHost", bad, err)
		}
	}
	if _, err := m.CreateEnrollToken(ctx, hostID); err != nil {
		t.Fatalf("CreateEnrollToken with a real host: %v", err)
	}
}

// Two clusters that each contain a node called "pve1" must end up with two
// registrations, neither of which displaces the other. This is the defect that
// could route a backup to the wrong physical machine.
func TestTwoClustersWithTheSameNodeNameCoexist(t *testing.T) {
	ctx := context.Background()
	m, st, hostA := newManager(t)
	otherHost, err := st.CreatePVEHost(ctx, &store.PVEHost{
		Name: "cluster-b", BaseURL: "https://pve-b.example:8006",
		TokenID: "root@pam!proxback", TokenSecret: "secret", Status: "online",
	})
	if err != nil {
		t.Fatalf("create second host: %v", err)
	}

	register := func(hostID, secret, addr string) string {
		t.Helper()
		tok, err := m.CreateEnrollToken(ctx, hostID)
		if err != nil {
			t.Fatalf("create enroll token: %v", err)
		}
		res, err := m.Register(ctx, helpermgr.RegisterRequest{
			Token: tok.Token, Node: "pve1", AccessSecret: secret, Version: "0.5.0",
		}, addr)
		if err != nil {
			t.Fatalf("register helper for %s: %v", hostID, err)
		}
		return res.HelperID
	}
	idA := register(hostA, "secret-a", "10.0.0.11:1")
	idB := register(otherHost.ID, "secret-b", "10.9.9.11:1")
	if idA == idB {
		t.Fatal("the second registration reused the first helper")
	}

	helpers, err := st.ListHelpers(ctx)
	if err != nil || len(helpers) != 2 {
		t.Fatalf("helpers = %+v (%v), want both registrations", helpers, err)
	}
	a, err := st.HelperFor(ctx, hostA, "pve1")
	if err != nil || a.ID != idA || a.AccessSecret != "secret-a" || a.Address != "10.0.0.11" {
		t.Fatalf("cluster-a pve1 = %+v (%v)", a, err)
	}
	b, err := st.HelperFor(ctx, otherHost.ID, "pve1")
	if err != nil || b.ID != idB || b.AccessSecret != "secret-b" || b.Address != "10.9.9.11" {
		t.Fatalf("cluster-b pve1 = %+v (%v)", b, err)
	}
}

// A registration with no host is reported as unassigned however healthy it
// looks, because nothing can say which cluster's node it is.
func TestUnassignedHelperStatusOutranksLiveness(t *testing.T) {
	now := time.Now()
	if got := helpermgr.Status(&store.NodeHelper{LastSeen: &now}); got != store.HelperUnassigned {
		t.Fatalf("a heartbeating helper with no host is %q, want %q", got, store.HelperUnassigned)
	}
	if got := helpermgr.Status(&store.NodeHelper{HostID: "h1", LastSeen: &now}); got != "online" {
		t.Fatalf("an assigned, heartbeating helper is %q", got)
	}
}

func TestAddress(t *testing.T) {
	for remote, want := range map[string]string{
		"10.0.0.12:54321":       "10.0.0.12",
		"[fd00::1]:8007":        "fd00::1",
		"127.0.0.1:1":           "127.0.0.1",
		"no-port-here":          "no-port-here",
		"proxback.example:9999": "proxback.example",
	} {
		if got := helpermgr.Address(remote); got != want {
			t.Errorf("Address(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestOnlineWindow(t *testing.T) {
	if helpermgr.Online(nil) {
		t.Fatal("a nil helper is online")
	}
	if helpermgr.Status(&store.NodeHelper{HostID: "h1"}) != "offline" {
		t.Fatal("a never-seen helper is not offline")
	}
	// Two missed 30 s heartbeats are still within tolerance; a helper unseen for
	// longer is not.
	recent := time.Now().Add(-helpermgr.OnlineWindow + 5*time.Second)
	if !helpermgr.Online(&store.NodeHelper{LastSeen: &recent}) {
		t.Fatalf("a helper seen %s ago is offline", time.Since(recent))
	}
	stale := time.Now().Add(-helpermgr.OnlineWindow - time.Second)
	if helpermgr.Online(&store.NodeHelper{LastSeen: &stale}) {
		t.Fatalf("a helper seen %s ago is still online", time.Since(stale))
	}
}

func TestHeartbeatMovesLastSeen(t *testing.T) {
	ctx := context.Background()
	m, st, hostID := newManager(t)
	old := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	h, err := st.CreateHelper(ctx, &store.NodeHelper{
		HostID: hostID, Node: "pve2", Address: "10.0.0.12", Version: "0.3.0",
		AccessSecret: "s", APIKeyHash: "h", LastSeen: &old,
	})
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	if helpermgr.Online(h) {
		t.Fatal("an hour-old helper is online")
	}
	if err := m.Heartbeat(ctx, h.ID, helpermgr.HeartbeatRequest{}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	beaten, err := st.HelperByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("reload helper: %v", err)
	}
	if !helpermgr.Online(beaten) {
		t.Fatalf("helper still offline after a heartbeat (lastSeen %v)", beaten.LastSeen)
	}
	// A heartbeat that says nothing about the version must not erase the one on
	// record: a helper old enough not to report it keeps what it enrolled with.
	if beaten.Version != "0.3.0" {
		t.Fatalf("version after a silent heartbeat = %q, want 0.3.0", beaten.Version)
	}
}

// TestHeartbeatRefreshesTheReportedVersion is the fix for a node helper that
// updated itself and went on being displayed as the build it was deployed with.
func TestHeartbeatRefreshesTheReportedVersion(t *testing.T) {
	ctx := context.Background()
	m, st, hostID := newManager(t)
	h, err := st.CreateHelper(ctx, &store.NodeHelper{
		HostID: hostID, Node: "pve3", Address: "10.0.0.13", Version: "0.6.1",
		AccessSecret: "s", APIKeyHash: "h",
	})
	if err != nil {
		t.Fatalf("create helper: %v", err)
	}
	if err := m.Heartbeat(ctx, h.ID, helpermgr.HeartbeatRequest{Version: "0.6.2"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	beaten, err := st.HelperByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("reload helper: %v", err)
	}
	if beaten.Version != "0.6.2" {
		t.Fatalf("version after an upgrade heartbeat = %q, want 0.6.2", beaten.Version)
	}
	// A downgrade is drift too, and must be just as visible.
	if err := m.Heartbeat(ctx, h.ID, helpermgr.HeartbeatRequest{Version: "0.6.0"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	beaten, err = st.HelperByID(ctx, h.ID)
	if err != nil {
		t.Fatalf("reload helper: %v", err)
	}
	if beaten.Version != "0.6.0" {
		t.Fatalf("version after a downgrade heartbeat = %q, want 0.6.0", beaten.Version)
	}
}
