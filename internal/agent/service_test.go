package agent

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeController stands in for the Windows Service Control Manager so the
// install and uninstall logic can be exercised on any platform and without
// elevation.
type fakeController struct {
	mu sync.Mutex

	state   serviceState
	spec    *serviceSpec
	created int
	started int
	stopped int
	deleted int

	eventSourceInstalled bool
	eventInstalls        int
	eventRemovals        int

	closed bool

	// startsInto is the state Start leaves the service in. Setting it to
	// serviceStartPending reproduces a service that is launched but never
	// completes the service control manager handshake — the error 1053 case.
	startsInto serviceState

	statusErr       error
	eventInstallErr error
}

func newFakeController() *fakeController {
	return &fakeController{state: serviceAbsent, startsInto: serviceRunning}
}

func (f *fakeController) Status(string) (serviceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return serviceUnknown, f.statusErr
	}
	return f.state, nil
}

func (f *fakeController) Create(spec serviceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := spec
	f.spec = &s
	f.created++
	f.state = serviceStopped
	return nil
}

func (f *fakeController) Start(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	f.state = f.startsInto
	return nil
}

func (f *fakeController) Stop(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	f.state = serviceStopped
	return nil
}

func (f *fakeController) Delete(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted++
	f.state = serviceAbsent
	return nil
}

func (f *fakeController) RegisterEventSource(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventInstalls++
	if f.eventInstallErr != nil {
		return f.eventInstallErr
	}
	f.eventSourceInstalled = true
	return nil
}

func (f *fakeController) RemoveEventSource(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventRemovals++
	f.eventSourceInstalled = false
	return nil
}

func (f *fakeController) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func testPoll() servicePoll {
	return servicePoll{Interval: time.Millisecond, Timeout: 50 * time.Millisecond}
}

func testSpec() serviceSpec {
	return serviceSpec{
		Name:         ServiceName,
		DisplayName:  ServiceDisplayName,
		Description:  ServiceDescription,
		BinaryPath:   `C:\Program Files\ProxBack\proxback-agent.exe`,
		Args:         []string{"--config", `C:\ProgramData\ProxBack`},
		RestartDelay: RestartDelay,
		ResetPeriod:  FailureResetPeriod,
	}
}

func TestInstallServiceRegistersStartsAndVerifies(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	var out strings.Builder
	if err := installService(&out, f, testSpec(), testPoll()); err != nil {
		t.Fatalf("installService: %v", err)
	}
	if f.created != 1 || f.started != 1 {
		t.Fatalf("created=%d started=%d, want 1/1", f.created, f.started)
	}
	if !f.eventSourceInstalled {
		t.Fatal("event log source was not registered")
	}
	if f.spec == nil || f.spec.RestartDelay != RestartDelay || f.spec.ResetPeriod != FailureResetPeriod {
		t.Fatalf("failure actions not carried through: %+v", f.spec)
	}
	if !strings.Contains(out.String(), "is running") {
		t.Fatalf("install output does not confirm the service is running:\n%s", out.String())
	}
}

func TestInstallServiceFailsWhenServiceNeverReachesRunning(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	// The service launches but never reports Running: exactly what a plain
	// console binary does under the SCM, and what error 1053 means.
	f.startsInto = serviceStartPending
	var out strings.Builder
	err := installService(&out, f, testSpec(), testPoll())
	if err == nil {
		t.Fatal("installService = nil, want an error when the service never runs")
	}
	for _, want := range []string{"did not reach the running state", "start pending", "Event Log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestInstallServiceReplacesExistingRegistration(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	var out strings.Builder
	if err := installService(&out, f, testSpec(), testPoll()); err != nil {
		t.Fatalf("first installService: %v", err)
	}
	if err := installService(&out, f, testSpec(), testPoll()); err != nil {
		t.Fatalf("second installService: %v", err)
	}
	if f.created != 2 {
		t.Fatalf("created = %d, want 2", f.created)
	}
	if f.stopped != 1 || f.deleted != 1 {
		t.Fatalf("stopped=%d deleted=%d, want the running service to be stopped and deleted once", f.stopped, f.deleted)
	}
	if !strings.Contains(out.String(), "replacing it") {
		t.Fatalf("output does not mention replacing the old registration:\n%s", out.String())
	}
}

func TestInstallServiceSurvivesEventLogRegistrationFailure(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	f.eventInstallErr = errors.New("access is denied")
	var out strings.Builder
	if err := installService(&out, f, testSpec(), testPoll()); err != nil {
		t.Fatalf("installService: %v", err)
	}
	if !strings.Contains(out.String(), "warning: could not register the event log source") {
		t.Fatalf("event log failure not surfaced:\n%s", out.String())
	}
}

func TestInstallServiceRejectsIncompleteSpec(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	spec := testSpec()
	spec.BinaryPath = ""
	if err := installService(io.Discard, f, spec, testPoll()); err == nil {
		t.Fatal("installService with no binary path = nil, want error")
	}
	if f.created != 0 {
		t.Fatalf("created = %d, want 0", f.created)
	}
}

func TestUninstallServiceIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	if err := installService(io.Discard, f, testSpec(), testPoll()); err != nil {
		t.Fatalf("installService: %v", err)
	}

	var first strings.Builder
	if err := uninstallService(&first, f, ServiceName, testPoll()); err != nil {
		t.Fatalf("first uninstallService: %v", err)
	}
	if f.deleted != 1 || f.stopped != 1 {
		t.Fatalf("deleted=%d stopped=%d, want 1/1", f.deleted, f.stopped)
	}
	if f.eventSourceInstalled {
		t.Fatal("event log source survived uninstall")
	}

	var second strings.Builder
	if err := uninstallService(&second, f, ServiceName, testPoll()); err != nil {
		t.Fatalf("second uninstallService: %v", err)
	}
	if f.deleted != 1 {
		t.Fatalf("deleted = %d after a second uninstall, want 1", f.deleted)
	}
	if !strings.Contains(second.String(), "is not installed") {
		t.Fatalf("second uninstall should say the service is gone:\n%s", second.String())
	}
	// Removing the event source again is harmless and still attempted.
	if f.eventRemovals != 2 {
		t.Fatalf("eventRemovals = %d, want 2", f.eventRemovals)
	}
}

func TestUninstallServiceOnCleanMachineSucceeds(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	var out strings.Builder
	if err := uninstallService(&out, f, ServiceName, testPoll()); err != nil {
		t.Fatalf("uninstallService on a machine that never had the agent: %v", err)
	}
	if f.stopped != 0 || f.deleted != 0 {
		t.Fatalf("stopped=%d deleted=%d, want nothing touched", f.stopped, f.deleted)
	}
}

func TestUninstallServiceReportsQueryFailure(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	f.statusErr = errors.New("access is denied")
	err := uninstallService(io.Discard, f, ServiceName, testPoll())
	if err == nil || !strings.Contains(err.Error(), "access is denied") {
		t.Fatalf("uninstallService = %v, want the query failure surfaced", err)
	}
}

func TestWaitForStateTreatsAbsentAsStopped(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	if err := waitForState(f, ServiceName, serviceStopped, testPoll()); err != nil {
		t.Fatalf("waitForState(stopped) on an absent service = %v, want nil", err)
	}
	if err := waitForState(f, ServiceName, serviceRunning, testPoll()); err == nil {
		t.Fatal("waitForState(running) on an absent service = nil, want a timeout")
	}
}

func TestWaitForStateSurfacesStatusErrors(t *testing.T) {
	t.Parallel()
	f := newFakeController()
	f.statusErr = errors.New("rpc unavailable")
	if err := waitForState(f, ServiceName, serviceRunning, testPoll()); !errors.Is(err, f.statusErr) {
		t.Fatalf("waitForState = %v, want the status error", err)
	}
}

func TestRunningAsServiceIsFalseUnderGoTest(t *testing.T) {
	t.Parallel()
	// The test binary is started from a console on every platform, so the
	// interactive path must be chosen. This is the decision that keeps
	// --once and normal foreground runs behaving as before.
	isService, err := RunningAsService()
	if err != nil {
		t.Fatalf("RunningAsService: %v", err)
	}
	if isService {
		t.Fatal("RunningAsService() = true under go test, want false")
	}
}
