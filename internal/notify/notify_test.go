package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"proxback/internal/notify"
)

func samplePayload() notify.Payload {
	started := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	return notify.Payload{
		Event:          notify.EventRunFinished,
		Server:         "lab",
		Job:            "nightly-vms",
		Kind:           "vm",
		Status:         "success",
		BytesProcessed: 48 << 20,
		BytesUploaded:  4 << 20,
		DedupRatio:     0.75,
		StartedAt:      started,
		FinishedAt:     started.Add(90 * time.Second),
		DurationSec:    90,
	}
}

func TestSendPostsContractPayload(t *testing.T) {
	type received struct {
		method      string
		contentType string
		body        []byte
	}
	got := make(chan received, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got <- received{method: r.Method, contentType: r.Header.Get("Content-Type"), body: raw}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := notify.New(nil).Send(context.Background(), srv.URL, samplePayload()); err != nil {
		t.Fatalf("send: %v", err)
	}
	r := <-got
	if r.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", r.method)
	}
	if !strings.HasPrefix(r.contentType, "application/json") {
		t.Fatalf("content type = %q", r.contentType)
	}
	// Field names are part of the REST contract, so assert on the raw JSON keys.
	var decoded map[string]any
	if err := json.Unmarshal(r.body, &decoded); err != nil {
		t.Fatalf("decode payload %s: %v", r.body, err)
	}
	for _, key := range []string{
		"event", "server", "job", "kind", "status", "bytesProcessed",
		"bytesUploaded", "dedupRatio", "startedAt", "finishedAt", "durationSec",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("payload is missing %q: %s", key, r.body)
		}
	}
	// error is omitted when the run succeeded.
	if _, ok := decoded["error"]; ok {
		t.Errorf("successful payload carries an error field: %s", r.body)
	}
	if decoded["event"] != notify.EventRunFinished {
		t.Errorf("event = %v", decoded["event"])
	}
	if decoded["startedAt"] != "2026-07-27T01:00:00Z" {
		t.Errorf("startedAt = %v, want RFC3339 UTC", decoded["startedAt"])
	}
}

func TestSendReportsFailures(t *testing.T) {
	n := notify.New(nil)

	if err := n.Send(context.Background(), "", samplePayload()); err == nil {
		t.Fatal("send with no url succeeded")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := n.Send(context.Background(), srv.URL, samplePayload())
	if err == nil || !strings.Contains(err.Error(), "http 500") {
		t.Fatalf("send to a failing endpoint = %v, want an http 500 error", err)
	}

	// A non-2xx or unreachable endpoint must never panic or block; Notify
	// swallows the error entirely.
	n.Notify(context.Background(), srv.URL, samplePayload())

	// Errors carry the failing status, and a payload with an error keeps it.
	p := samplePayload()
	p.Status = "failed"
	p.Error = "snapshot web-01: boom"
	var seen notify.Payload
	body := make(chan notify.Payload, 1)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got notify.Payload
		_ = json.NewDecoder(r.Body).Decode(&got)
		body <- got
		w.WriteHeader(http.StatusOK)
	}))
	defer ok.Close()
	if err := n.Send(context.Background(), ok.URL, p); err != nil {
		t.Fatalf("send failure payload: %v", err)
	}
	seen = <-body
	if seen.Status != "failed" || seen.Error != "snapshot web-01: boom" {
		t.Fatalf("failure payload round trip = %+v", seen)
	}
}
