package sched

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"proxback/internal/engine"
	"proxback/internal/helpermgr"
	"proxback/internal/pve"
	"proxback/internal/store"
)

// VMAStreamName is the manifest stream name of a whole-guest vzdump archive.
// A helper-backed restore point carries exactly one stream with this name; the
// legacy per-disk path names its streams after the guest's disk keys (scsi0…).
const VMAStreamName = "vma"

// NoHelperHint is the operator guidance shown when a node can neither be backed
// up by a helper (none registered) nor by the export extension (real Proxmox has
// no such API). It names the node because a cluster is usually mixed while the
// helper is being rolled out.
const NoHelperHint = "this Proxmox node has no ProxBack node helper installed — " +
	"open Hosts → Node helpers to deploy it"

// IsVMAManifest reports whether a restore point was produced by a node helper,
// i.e. it is a single whole-guest vzdump archive rather than per-disk streams.
func IsVMAManifest(disks []engine.DiskManifest) bool {
	return len(disks) == 1 && disks[0].Name == VMAStreamName
}

// UnassignedHelperHint is shown when the only helper carrying a node's name is
// not bound to a Proxmox host. Two clusters can each contain a "pve1", so using
// it would be a guess about which physical machine is meant — and the wrong
// guess sends a backup, or a restore, to someone else's hardware.
const UnassignedHelperHint = "a ProxBack node helper is registered for this node name but is not " +
	"assigned to a Proxmox host, so it cannot be used — open Hosts → Node helpers and assign it " +
	"to the right host, or re-deploy it"

// noHelperError renders NoHelperHint for a node.
func noHelperError(node string) error {
	return fmt.Errorf("node %q: %s", node, NoHelperHint)
}

// unassignedHelperError renders UnassignedHelperHint for a node.
func unassignedHelperError(node string) error {
	return fmt.Errorf("node %q: %s", node, UnassignedHelperHint)
}

// mapExportError turns the Proxmox extension's "there is no such endpoint"
// answer into the actionable install-the-helper message. Real Proxmox answers
// 501 (or 404 behind some proxies) for /proxback-export, which without this
// mapping surfaces to the operator as a bare `http 501`.
func mapExportError(err error, node string) error {
	var apiErr *pve.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Status != http.StatusNotImplemented && apiErr.Status != http.StatusNotFound {
		return err
	}
	return fmt.Errorf("node %q: %s: %w", node, NoHelperHint, err)
}

// helperForNode resolves the helper that owns a node of one Proxmox host. The
// host is half of the lookup key: resolving by node name alone would route a
// backup or a restore to the identically named node of another cluster.
//
// A registered but offline helper is an error rather than a silent fall back to
// the extension path: the operator installed it, so backing up behind its back
// would be a lie. So is a helper that carries the node's name but no host —
// that one needs an operator decision, not a guess.
func (m *Manager) helperForNode(ctx context.Context, hostID, node string) (*store.NodeHelper, error) {
	h, err := m.st.HelperFor(ctx, hostID, node)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Nothing serves this (host, node). If something is registered for the
		// node name without a host, say so instead of quietly falling back.
		if _, uerr := m.st.UnassignedHelperForNode(ctx, node); uerr == nil {
			return nil, unassignedHelperError(node)
		} else if !errors.Is(uerr, store.ErrNotFound) {
			return nil, fmt.Errorf("look up node helper for %q: %w", node, uerr)
		}
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("look up node helper for %q: %w", node, err)
	}
	if !helpermgr.Online(h) {
		return nil, fmt.Errorf("helper for node %s is offline", node)
	}
	return h, nil
}

// requireHelperForNode resolves the helper that must handle a node, failing with
// the actionable message when none is registered.
func (m *Manager) requireHelperForNode(ctx context.Context, hostID, node string) (*store.NodeHelper, error) {
	h, err := m.helperForNode(ctx, hostID, node)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, noHelperError(node)
	}
	return h, nil
}

// backupViaHelper streams a whole guest through the helper's vzdump export as a
// single manifest stream. No PVE snapshot is created or deleted: vzdump owns
// consistency (and, with --mode snapshot, its own snapshot lifecycle).
func (m *Manager) backupViaHelper(ctx context.Context, runID string, sess *engine.Session, p vmPlan) ([]engine.DiskManifest, error) {
	sess.SetStep(fmt.Sprintf("Backing up %s (vzdump stream)", p.name))
	body, err := m.helperClient.Export(ctx, p.helper.Address, p.helper.Port, p.helper.AccessSecret, p.vmid)
	if err != nil {
		return nil, fmt.Errorf("export %s via node helper on %s: %w", p.name, p.node, err)
	}
	// A vzdump that dies mid-run leaves a truncated HTTP body. io.ReadFull — and
	// therefore BackupStream — cannot tell that from a short final chunk, so the
	// transport error is recorded here and consulted afterwards.
	tracked := &errTrackingReader{r: body}
	dm, err := sess.BackupStream(ctx, VMAStreamName, tracked)
	closeErr := body.Close()
	if err != nil {
		return nil, err
	}
	if tracked.err != nil {
		return nil, fmt.Errorf("export %s via node helper on %s: stream ended early after %d bytes: %w",
			p.name, p.node, dm.SizeBytes, tracked.err)
	}
	if closeErr != nil {
		m.log.Warn("closing node helper export stream", "vm", p.vmid, "node", p.node, "error", closeErr)
		m.logRun(ctx, runID, "warning: %s: closing the helper export stream failed: %v", p.name, closeErr)
	}
	if dm.SizeBytes == 0 {
		return nil, fmt.Errorf("export %s via node helper on %s: vzdump produced no data", p.name, p.node)
	}
	return []engine.DiskManifest{dm}, nil
}

// restoreViaHelper pipes a stored VMA archive into qmrestore on the node.
func (m *Manager) restoreViaHelper(ctx context.Context, sess *engine.Session, man *engine.Manifest,
	h *store.NodeHelper, vmid int, storage string, force bool) error {
	disk := man.Disks[0]
	sess.SetStep(fmt.Sprintf("Restoring %s to vm %d (qmrestore stream)", man.SourceName, vmid))
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := sess.RestoreDisk(ctx, disk, pw)
		_ = pw.CloseWithError(err)
		errc <- err
	}()
	importErr := m.helperClient.Import(ctx, h.Address, h.Port, h.AccessSecret, vmid, storage, force, pr)
	_ = pr.Close()
	restoreErr := <-errc
	// When the helper rejects the stream the engine goroutine dies with a closed
	// pipe error; the helper's complaint is the one worth reporting.
	if restoreErr != nil && !errors.Is(restoreErr, io.ErrClosedPipe) {
		return restoreErr
	}
	if importErr != nil {
		return fmt.Errorf("restore %s via node helper on %s: %w", man.SourceName, h.Node, importErr)
	}
	return restoreErr
}

// errTrackingReader remembers the first non-EOF read error it saw. It exists
// because a truncated HTTP body reports io.ErrUnexpectedEOF, which the chunking
// reader legitimately treats as the end of a stream.
type errTrackingReader struct {
	r   io.Reader
	err error
}

func (t *errTrackingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && t.err == nil {
		t.err = err
	}
	return n, err
}
