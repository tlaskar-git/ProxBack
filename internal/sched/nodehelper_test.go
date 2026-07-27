package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"proxback/internal/agentmgr"
	"proxback/internal/engine"
	"proxback/internal/pve"
	"proxback/internal/store"
)

// TestMapExportErrorExplainsAMissingNodeHelper covers the failure every operator
// hits on their first real (non-simulated) Proxmox host: the disk export
// extension does not exist there, so a bare "http 501" has to become an
// instruction.
func TestMapExportErrorExplainsAMissingNodeHelper(t *testing.T) {
	for _, status := range []int{http.StatusNotImplemented, http.StatusNotFound} {
		original := &pve.APIError{
			Method: http.MethodGet,
			Path:   "/api2/json/nodes/pve1/qemu/100/proxback-export/scsi0",
			Status: status,
			Body:   `{"data":null}`,
		}
		err := mapExportError(original, "pve1")
		if err == nil {
			t.Fatalf("http %d mapped to no error", status)
		}
		want := `node "pve1": this Proxmox node has no ProxBack node helper installed — ` +
			`open Hosts → Node helpers to deploy it`
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("http %d mapped to %q, want it to contain %q", status, err, want)
		}
		// The original stays reachable as the detail.
		var apiErr *pve.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Fatalf("mapped error lost the underlying APIError: %v", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("http %d", status)) {
			t.Fatalf("mapped error hides the original detail: %q", err)
		}
	}
}

func TestMapExportErrorLeavesOtherFailuresAlone(t *testing.T) {
	for _, original := range []error{
		&pve.APIError{Method: http.MethodGet, Path: "/x", Status: http.StatusUnauthorized, Body: "no ticket"},
		&pve.APIError{Method: http.MethodGet, Path: "/x", Status: http.StatusInternalServerError, Body: "boom"},
		errors.New("dial tcp 10.0.0.1:8006: connect: connection refused"),
	} {
		if got := mapExportError(original, "pve1"); got != original {
			t.Fatalf("mapExportError rewrote %v into %v", original, got)
		}
	}
}

func TestIsVMAManifest(t *testing.T) {
	for _, c := range []struct {
		what  string
		disks []engine.DiskManifest
		want  bool
	}{
		{"a helper backup", []engine.DiskManifest{{Name: VMAStreamName}}, true},
		{"a legacy single-disk backup", []engine.DiskManifest{{Name: "scsi0"}}, false},
		{"a legacy multi-disk backup", []engine.DiskManifest{{Name: "scsi0"}, {Name: "scsi1"}}, false},
		{"a mixed manifest", []engine.DiskManifest{{Name: VMAStreamName}, {Name: "scsi1"}}, false},
		{"an empty manifest", nil, false},
	} {
		if got := IsVMAManifest(c.disks); got != c.want {
			t.Errorf("IsVMAManifest(%s) = %v, want %v", c.what, got, c.want)
		}
	}
}

// TestHelperForNode covers the three states a node can be in: no helper (fall
// back to the extension), a live helper, and a helper that has stopped
// heartbeating — which must fail loudly rather than silently take the old path.
func TestHelperForNode(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	m := New(st, agentmgr.New(st, discardLog()), discardLog())

	// No helper: not an error, the caller tries the export extension.
	h, err := m.helperForNode(ctx, "pve1")
	if err != nil || h != nil {
		t.Fatalf("helperForNode without a helper = %+v (%v)", h, err)
	}
	if _, err := m.requireHelperForNode(ctx, "pve1"); err == nil ||
		!strings.Contains(err.Error(), NoHelperHint) {
		t.Fatalf("requireHelperForNode without a helper = %v", err)
	}

	fresh := time.Now().UTC()
	if _, err := st.CreateHelper(ctx, &store.NodeHelper{
		Node: "pve1", Address: "10.0.0.11", Port: 8007, Version: "0.3.0",
		AccessSecret: "secret", APIKeyHash: "hash", LastSeen: &fresh,
	}); err != nil {
		t.Fatalf("create helper: %v", err)
	}
	if h, err = m.helperForNode(ctx, "pve1"); err != nil || h == nil || h.AccessSecret != "secret" {
		t.Fatalf("helperForNode with a live helper = %+v (%v)", h, err)
	}
	if h, err = m.requireHelperForNode(ctx, "pve1"); err != nil || h.Node != "pve1" {
		t.Fatalf("requireHelperForNode with a live helper = %+v (%v)", h, err)
	}

	// Two missed heartbeats is still online; a helper unseen for minutes is not.
	stale := time.Now().UTC().Add(-10 * time.Minute)
	if err := st.TouchHelper(ctx, h.ID, stale); err != nil {
		t.Fatalf("age the helper: %v", err)
	}
	if _, err := m.helperForNode(ctx, "pve1"); err == nil ||
		!strings.Contains(err.Error(), "helper for node pve1 is offline") {
		t.Fatalf("helperForNode with a stale helper = %v, want an offline error", err)
	}
	if _, err := m.requireHelperForNode(ctx, "pve1"); err == nil ||
		!strings.Contains(err.Error(), "offline") {
		t.Fatalf("requireHelperForNode with a stale helper = %v", err)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// truncatedReader serves n bytes and then fails the way net/http reports a
// connection the helper aborted mid-vzdump.
type truncatedReader struct{ left int }

func (t *truncatedReader) Read(p []byte) (int, error) {
	if t.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p)
	if n > t.left {
		n = t.left
	}
	for i := range p[:n] {
		p[i] = 'v'
	}
	t.left -= n
	return n, nil
}

// TestErrTrackingReaderCatchesATruncatedStream pins down the subtle reason the
// wrapper exists: io.ReadFull reports a truncated stream as io.ErrUnexpectedEOF,
// which is indistinguishable from a legitimate short final chunk, so the chunker
// would treat a half-transferred vzdump archive as a complete backup.
func TestErrTrackingReaderCatchesATruncatedStream(t *testing.T) {
	tracked := &errTrackingReader{r: &truncatedReader{left: 100}}
	buf := make([]byte, 64)

	n, err := io.ReadFull(tracked, buf)
	if n != 64 || err != nil {
		t.Fatalf("first full read = %d, %v", n, err)
	}
	if tracked.err != nil {
		t.Fatalf("a healthy read recorded %v", tracked.err)
	}
	// The second read is short and ends in failure — which io.ReadFull renders as
	// a plain ErrUnexpectedEOF, exactly like a valid last chunk.
	n, err = io.ReadFull(tracked, buf)
	if n != 36 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated read = %d, %v", n, err)
	}
	if !errors.Is(tracked.err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncation was not recorded: %v", tracked.err)
	}

	// A clean end of stream must not be recorded as a failure.
	clean := &errTrackingReader{r: strings.NewReader("vma")}
	if _, err := io.ReadAll(clean); err != nil {
		t.Fatalf("read a clean stream: %v", err)
	}
	if clean.err != nil {
		t.Fatalf("a clean EOF was recorded as %v", clean.err)
	}
}

func TestNoHelperErrorNamesTheNode(t *testing.T) {
	err := noHelperError("pve2")
	if !strings.Contains(err.Error(), `node "pve2"`) || !strings.Contains(err.Error(), NoHelperHint) {
		t.Fatalf("noHelperError = %q", err)
	}
}
