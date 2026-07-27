package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"proxback/internal/store"
)

// An enrollment token is minted for a host. Without one there is no token,
// because the helper it would create could never be routed to.
func TestHelperEnrollTokenRequiresAHost(t *testing.T) {
	ts := newTestServer(t)

	for _, body := range []any{
		map[string]any{},
		map[string]any{"hostId": ""},
		map[string]any{"hostId": "no-such-host"},
	} {
		code, got := ts.post(t, "/api/helpers/enroll-token", body)
		if code != http.StatusBadRequest {
			t.Fatalf("enroll-token %v = %d (%+v), want 400", body, code, got)
		}
		if msg, _ := got["error"].(string); !strings.Contains(msg, "hostId") {
			t.Errorf(`response["error"] = %q, want it to name hostId`, msg)
		}
	}

	code, got := ts.post(t, "/api/helpers/enroll-token", map[string]any{"hostId": ts.hostID})
	if code != http.StatusOK {
		t.Fatalf("enroll-token with a host = %d (%+v)", code, got)
	}
	token, _ := got["token"].(string)
	if token == "" || got["expiresAt"] == nil {
		t.Fatalf("enroll token response = %+v", got)
	}
	stored, err := ts.st.EnrollTokenByValue(context.Background(), token)
	if err != nil {
		t.Fatalf("load minted token: %v", err)
	}
	if stored.HostID != ts.hostID || stored.Purpose != store.EnrollPurposeHelper {
		t.Fatalf("minted token = %+v, want it bound to the host", stored)
	}
}

// The listing carries the identity the console needs to tell two clusters'
// "pve1" apart, and reports a hostless registration as unassigned.
func TestListHelpersReportsIdentityAndUnassigned(t *testing.T) {
	ts := newTestServer(t)
	ctx := context.Background()
	now := store.Now()

	if _, err := ts.st.CreateHelper(ctx, &store.NodeHelper{
		HostID: ts.hostID, Node: "pve1", Address: "10.0.0.11", Port: 8007,
		Version: "0.5.0", AccessSecret: "secret-a", APIKeyHash: "hash-a", LastSeen: &now,
	}); err != nil {
		t.Fatalf("create assigned helper: %v", err)
	}
	legacy, err := ts.st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve2", Address: "10.0.0.12", Port: 8007,
		Version: "0.4.0", AccessSecret: "secret-b", APIKeyHash: "hash-b", LastSeen: &now,
	})
	if err != nil {
		t.Fatalf("create legacy helper: %v", err)
	}

	code, raw := ts.getRaw(t, "/api/helpers")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", code, raw)
	}
	var helpers []helperDTO
	decodeJSONBody(t, raw, &helpers)
	if len(helpers) != 2 {
		t.Fatalf("helpers = %+v", helpers)
	}
	byNode := map[string]helperDTO{}
	for _, h := range helpers {
		byNode[h.Node] = h
	}
	assigned := byNode["pve1"]
	if assigned.HostID != ts.hostID || assigned.HostName != "cluster-a" || assigned.Status != "online" {
		t.Fatalf("assigned helper = %+v", assigned)
	}
	unassigned := byNode["pve2"]
	if unassigned.HostID != "" || unassigned.HostName != "" ||
		unassigned.Status != store.HelperUnassigned {
		t.Fatalf("legacy helper = %+v, want it reported unassigned", unassigned)
	}

	// Assigning it binds it to a host without a redeployment.
	code, got := ts.post(t, "/api/helpers/"+legacy.ID+"/assign", map[string]any{"hostId": ts.hostID})
	if code != http.StatusOK {
		t.Fatalf("assign = %d (%+v)", code, got)
	}
	if got["hostId"] != ts.hostID || got["status"] != "online" {
		t.Fatalf("assigned helper = %+v", got)
	}
	bound, err := ts.st.HelperFor(ctx, ts.hostID, "pve2")
	if err != nil || bound.ID != legacy.ID {
		t.Fatalf("helper after assignment = %+v (%v)", bound, err)
	}

	// Assignment validates its inputs and refuses to create a duplicate
	// (host, node) pair, which is the helper's identity.
	if code, _ := ts.post(t, "/api/helpers/"+legacy.ID+"/assign", map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("assign without a hostId = %d, want 400", code)
	}
	if code, _ := ts.post(t, "/api/helpers/"+legacy.ID+"/assign",
		map[string]any{"hostId": "nope"}); code != http.StatusNotFound {
		t.Fatalf("assign to an unknown host = %d, want 404", code)
	}
	if code, _ := ts.post(t, "/api/helpers/no-such-helper/assign",
		map[string]any{"hostId": ts.hostID}); code != http.StatusNotFound {
		t.Fatalf("assign of an unknown helper = %d, want 404", code)
	}
	clash, err := ts.st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve1", Address: "10.0.0.13", AccessSecret: "s", APIKeyHash: "hash-c",
	})
	if err != nil {
		t.Fatalf("create clashing helper: %v", err)
	}
	code, got = ts.post(t, "/api/helpers/"+clash.ID+"/assign", map[string]any{"hostId": ts.hostID})
	if code != http.StatusConflict {
		t.Fatalf("assigning a second pve1 to the same host = %d (%+v), want 409", code, got)
	}
}
