package api

// Fleet updates: keeping the agents and node helpers a server hands out in step
// with the server itself.
//
// The server has updated itself since 0.2.x and, since 0.6.1, has kept the
// binaries in <data>/downloads in step with its own version. What nothing ever
// did was update a component that was already installed. A user's server
// reached 0.6.2 while the Windows agent on a protected machine stayed on 0.6.1,
// kept failing on a bug fixed in 0.6.2, and was displayed in the console as
// version "1.0.0" — because an agent's version was recorded once, at
// registration, and never refreshed.
//
// So there are three parts, and all three are needed for any of them to be
// worth anything: every heartbeat now carries the running version (handled in
// agentmgr/helpermgr), every list row says whether that version is the server's
// (versionDrift below), and an operator can act on it from the console without
// visiting the guest (the endpoints below).
//
// The delivery mechanism is whatever each component already has. An agent polls
// /api/agents/heartbeat for work, so its update is one more dispatch it picks
// up on the next poll. A node helper is contacted directly by the server over
// its authenticated HTTP API, so its update is a call on that API. Neither
// component gains a new channel, a new port or a route to the internet: both
// fetch the binary from this server's own /downloads, which is already the
// build this server hands out to new installs.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"proxback/internal/agentmgr"
	"proxback/internal/helpermgr"
	"proxback/internal/store"
	"proxback/internal/update"
	"proxback/internal/version"
)

// versionDrift answers the two questions the console asks of every agent and
// node helper.
//
// They are deliberately not each other's negation. A component running a build
// newer than the server — a beta agent, a rollback in progress on the server —
// is neither up to date nor in need of an update, and saying "update available"
// there would offer a downgrade. A component whose version is unknown (an
// installation old enough never to have reported one) does count as needing an
// update, because "unknown" is exactly the state this whole feature exists to
// end.
func versionDrift(componentVersion string) (upToDate, updateAvailable bool) {
	cur := strings.TrimSpace(componentVersion)
	return cur != "" && cur == version.Version, update.Newer(cur, version.Version)
}

// stagedArtifact is the binary this server would hand a component, with what it
// measured on the file. The measurements travel with the instruction so the
// component can refuse anything that arrives truncated or rewritten.
type stagedArtifact struct {
	Name      string
	Sha256    string
	SizeBytes int64
}

// errNotStaged is returned when the binary a component needs is not present in
// <data>/downloads. It is a refusal, not a failure: instructing a guest to
// download something this server cannot serve would only break the guest.
var errNotStaged = errors.New("api: that binary is not staged on this server")

// inspectStagedArtifact measures the staged binary called name.
func (s *Server) inspectStagedArtifact(name string) (stagedArtifact, error) {
	out := stagedArtifact{Name: name}
	path := update.StagedPath(s.dataDir, name)
	f, err := os.Open(path) //nolint:gosec // name comes from update.StagedArtifacts
	if err != nil {
		return out, fmt.Errorf("%w: %s", errNotStaged, name)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return out, fmt.Errorf("%w: %s", errNotStaged, name)
	}
	sum := sha256.New()
	n, err := io.Copy(sum, f)
	if err != nil {
		return out, fmt.Errorf("api: read the staged %s: %w", name, err)
	}
	if n == 0 {
		return out, fmt.Errorf("%w: %s is empty", errNotStaged, name)
	}
	out.Sha256 = hex.EncodeToString(sum.Sum(nil))
	out.SizeBytes = n
	return out, nil
}

// agentAssetName is the staged binary for an agent's platform, or "" when this
// server stages nothing for it. It is resolved against update.StagedArtifacts
// rather than assembled from the OS and architecture, so a platform this server
// cannot serve is refused up front instead of dispatched and then failed in the
// guest.
func agentAssetName(goos, goarch string) string {
	for _, art := range update.StagedArtifacts() {
		if art.Kind == "agent" && art.GOOS == goos && art.GOARCH == goarch {
			return art.Name
		}
	}
	return ""
}

// updateRefusal is the operator-facing reason a component was not updated. It
// is shared by the single-component endpoints and the update-all sweeps so both
// say the same thing about the same situation.
type updateRefusal struct {
	code   int
	reason string
}

func (r updateRefusal) Error() string { return r.reason }

// prepareComponentUpdate performs the checks common to both kinds of component:
// is it already the server's build, and can this server actually serve the
// binary it would need.
func (s *Server) prepareComponentUpdate(current, asset string, force bool) (stagedArtifact, error) {
	if asset == "" {
		return stagedArtifact{}, updateRefusal{http.StatusConflict,
			"this server stages no binary for that platform, so there is nothing it could hand out"}
	}
	if upToDate, _ := versionDrift(current); upToDate && !force {
		return stagedArtifact{}, updateRefusal{http.StatusConflict,
			"already running " + version.Version + ", the same build as this server (pass ?force=1 to reinstall it anyway)"}
	}
	art, err := s.inspectStagedArtifact(asset)
	if err != nil {
		if errors.Is(err, errNotStaged) {
			return art, updateRefusal{http.StatusConflict,
				"the " + asset + " binary is not staged on this server, so there is nothing to hand out — " +
					"GET /api/downloads/status reports what is staged"}
		}
		return art, err
	}
	return art, nil
}

// writeUpdateRefusal answers a refusal with its own status, and anything else
// as a server error.
func (s *Server) writeUpdateRefusal(w http.ResponseWriter, err error) {
	var refusal updateRefusal
	if errors.As(err, &refusal) {
		writeError(w, refusal.code, refusal.reason)
		return
	}
	s.serverError(w, err)
}

// ---------------------------------------------------------------- agents

// updateAgent queues a self-update for one agent. The returned string is the
// note shown to the operator; the error is a refusal or a real failure.
func (s *Server) updateAgent(a *store.Agent, force bool) (stagedArtifact, error) {
	art, err := s.prepareComponentUpdate(a.Version, agentAssetName(a.OS, a.Arch), force)
	if err != nil {
		return art, err
	}
	err = s.agents.QueueUpdate(a.ID, agentmgr.Dispatch{
		Version:   version.Version,
		Asset:     art.Name,
		Sha256:    art.Sha256,
		SizeBytes: art.SizeBytes,
	})
	if errors.Is(err, agentmgr.ErrAgentBusy) {
		// The refusal that matters most. Applying an update restarts the agent,
		// and restarting it mid-backup throws away everything the guest has
		// uploaded so far — so the answer is no, with a reason, not a
		// best-effort attempt.
		return art, updateRefusal{http.StatusConflict,
			"this agent has a run in flight; updating it would restart it and abort that run — " +
				"wait for the run to finish, or cancel it first"}
	}
	return art, err
}

// handleUpdateAgent instructs one agent to update itself.
//
// The dispatch is queued, not delivered: the agent collects it on its next
// heartbeat, which for an offline agent means whenever it comes back. The
// response says so rather than pretending the update has happened — nothing
// here is evidence of anything, and the endpoint is careful to say what it is:
// the update is confirmed when the agent heartbeats at the new version, and not
// before.
func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := s.st.AgentByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "agent")
		return
	}
	art, err := s.updateAgent(a, r.URL.Query().Get("force") == "1")
	if err != nil {
		s.audit(r, store.AuditEntry{
			Action: store.AuditAgentUpdate, Result: store.AuditError,
			ObjectKind: "agent", ObjectID: a.ID, ObjectName: a.Hostname,
			Detail: "refused: " + err.Error(),
		})
		s.writeUpdateRefusal(w, err)
		return
	}
	online := agentmgr.Online(a)
	s.log.Info("agent self-update dispatched", "agentId", a.ID, "hostname", a.Hostname,
		"from", a.Version, "to", version.Version, "asset", art.Name, "online", online)
	s.audit(r, store.AuditEntry{
		Action: store.AuditAgentUpdate, ObjectKind: "agent",
		ObjectID: a.ID, ObjectName: a.Hostname,
		Detail: "dispatched an update from " + displayVersion(a.Version) + " to " + version.Version,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "agentId": a.ID, "asset": art.Name,
		"fromVersion": a.Version, "toVersion": version.Version,
		"online": online,
		"note":   agentDispatchNote(online),
	})
}

// agentDispatchNote is what the console should tell the operator. It exists so
// the "not yet" is stated in one place: an update is applied when the agent
// says it is running the new version, never because this endpoint returned 202.
func agentDispatchNote(online bool) string {
	base := "the agent applies this on its next heartbeat and reports the new version on the one after; " +
		"it is not updated until that version appears"
	if !online {
		return "this agent is offline, so the update is queued and will be applied when it comes back; " + base
	}
	return base
}

// displayVersion renders a version for an operator, naming the empty case
// rather than printing nothing.
func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "an unreported version"
	}
	return v
}

// handleUpdateAllAgents sweeps every agent that is behind this server.
//
// It is a convenience over the per-agent endpoint and shares every one of its
// checks: an agent with a run in flight is skipped with its reason rather than
// interrupted, and the sweep reports what it did and did not do.
func (s *Server) handleUpdateAllAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.st.ListAgents(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "1"
	dispatched := []string{}
	skipped := []map[string]string{}
	for _, a := range agents {
		if _, uerr := s.updateAgent(a, force); uerr != nil {
			skipped = append(skipped, map[string]string{
				"agentId": a.ID, "hostname": a.Hostname, "reason": uerr.Error(),
			})
			continue
		}
		dispatched = append(dispatched, a.ID)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditAgentUpdate, ObjectKind: "agent",
		Detail: fmt.Sprintf("dispatched updates to %s to %d of %d agent(s)",
			version.Version, len(dispatched), len(agents)),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "version": version.Version,
		"dispatched": dispatched, "skipped": skipped,
		"note": agentDispatchNote(true),
	})
}

// ---------------------------------------------------------------- node helpers

// updateHelper tells one node helper to update itself, over the API the server
// already uses to drive it.
func (s *Server) updateHelper(ctx context.Context, h *store.NodeHelper, force bool) (stagedArtifact, error) {
	art, err := s.prepareComponentUpdate(h.Version, helperBinaryName, force)
	if err != nil {
		return art, err
	}
	_, err = s.requestHelperUpdate(ctx, h, helpermgr.UpdateRequest{
		Version:   version.Version,
		Asset:     art.Name,
		Sha256:    art.Sha256,
		SizeBytes: art.SizeBytes,
	})
	switch {
	case errors.Is(err, helpermgr.ErrHelperBusy):
		// The helper is the authority on this: it knows whether vzdump or
		// qmrestore is running on its node, and the server does not.
		return art, updateRefusal{http.StatusConflict,
			"this node helper has an export, import or policy script in flight; updating it would " +
				"restart it and abort that run — wait for the run to finish, or cancel it first"}
	case errors.Is(err, helpermgr.ErrHelperUnreachable):
		return art, updateRefusal{http.StatusBadGateway,
			"this node helper could not be reached at " + h.Address + ": " + err.Error()}
	case err != nil:
		return art, updateRefusal{http.StatusBadGateway, err.Error()}
	}
	return art, nil
}

// handleUpdateHelper instructs one node helper to update itself.
//
// Unlike an agent's, this is synchronous: the helper is contacted rather than
// polling, so a failed download is reported here and now. It still answers 202
// rather than 200, because installing the binary is not the same as running it
// — that is settled by the version on the next heartbeat.
func (s *Server) handleUpdateHelper(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	h, err := s.st.HelperByID(r.Context(), id)
	if err != nil {
		s.notFoundOr(w, err, "node helper")
		return
	}
	art, err := s.updateHelper(r.Context(), h, r.URL.Query().Get("force") == "1")
	if err != nil {
		s.log.Warn("node helper self-update refused",
			"helperId", h.ID, "node", h.Node, "error", err)
		s.audit(r, store.AuditEntry{
			Action: store.AuditHelperUpdate, Result: store.AuditError,
			ObjectKind: "helper", ObjectID: h.ID, ObjectName: h.Node,
			Detail: "refused: " + err.Error(),
		})
		s.writeUpdateRefusal(w, err)
		return
	}
	s.log.Info("node helper self-update applied", "helperId", h.ID, "node", h.Node,
		"from", h.Version, "to", version.Version, "asset", art.Name)
	s.audit(r, store.AuditEntry{
		Action: store.AuditHelperUpdate, ObjectKind: "helper",
		ObjectID: h.ID, ObjectName: h.Node,
		Detail: "updated from " + displayVersion(h.Version) + " to " + version.Version,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "helperId": h.ID, "asset": art.Name,
		"fromVersion": h.Version, "toVersion": version.Version,
		"note": helperDispatchNote,
	})
}

// helperDispatchNote is the same honesty as agentDispatchNote: the binary is in
// place, the daemon is restarting, and neither of those is the update being
// confirmed.
const helperDispatchNote = "the new binary is installed and the node helper is restarting; " +
	"it is not updated until it heartbeats at the new version"

// handleUpdateAllHelpers sweeps every node helper that is behind this server.
func (s *Server) handleUpdateAllHelpers(w http.ResponseWriter, r *http.Request) {
	helpers, err := s.st.ListHelpers(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	force := r.URL.Query().Get("force") == "1"
	updated := []string{}
	skipped := []map[string]string{}
	for _, h := range helpers {
		if _, uerr := s.updateHelper(r.Context(), h, force); uerr != nil {
			skipped = append(skipped, map[string]string{
				"helperId": h.ID, "node": h.Node, "reason": uerr.Error(),
			})
			continue
		}
		updated = append(updated, h.ID)
	}
	s.audit(r, store.AuditEntry{
		Action: store.AuditHelperUpdate, ObjectKind: "helper",
		Detail: fmt.Sprintf("updated %d of %d node helper(s) to %s",
			len(updated), len(helpers), version.Version),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "version": version.Version,
		"updated": updated, "skipped": skipped,
		"note": helperDispatchNote,
	})
}

// ---------------------------------------------------------------- decoding

// decodeOptionalJSON reads a request body that is allowed to be absent, empty
// or wrong. It is used only by the heartbeat endpoints, where the body carries
// nothing the server needs in order to record liveness: refusing a heartbeat
// because an optional field would not parse would take an agent offline in the
// console over a detail nobody depends on.
func decodeOptionalJSON[T any](r *http.Request) T {
	var out T
	if r.Body == nil {
		return out
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&out)
	return out
}
