package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

type workspaceLaneFixture struct {
	nativeFixture
	server *httptest.Server
	runner runnerFixture
	lane   *workspacerunner.Lane
	source string
}

func newWorkspaceLaneFixture(t *testing.T) *workspaceLaneFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	service := openTestService(t, Config{
		DatabasePath: filepath.Join(t.TempDir(), "hub.db"),
		Workspace: &WorkspaceConfig{Enabled: true, PersonMaxOpen: 10, PlanMaxOpen: 10,
			Terminal: WorkspaceTerminalConfig{Enabled: true}},
	})
	f := &workspaceLaneFixture{nativeFixture: newNativeFixture(t, service, "", "lane")}
	policy := hubTestPolicy()
	approveHubTestPolicy(t, service, f.base+"/policy", policy)
	f.server = httptest.NewServer(service.echo)
	t.Cleanup(f.server.Close)

	f.runner = prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Collaborate,
		runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	f.runner.enroll(t)
	client, err := hubclient.New(hubclient.Config{
		URL: f.server.URL, HTTPClient: f.server.Client(),
		TokenSource: func() string { return f.runner.redemption.Credential },
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	support := workspacerunner.Support{Terminal: true}
	if err := native.HeartbeatMachine(t.Context(), hubclient.Machine{
		ID: f.runner.binding.MachineID, Hostname: "runner-host", DisplayName: "runner", Capacity: 4, Version: "test",
		WorkspaceCapabilities: workspacerunner.Capabilities(support), WorkspaceIsolation: workspacesession.IsolationUser,
	}); err != nil {
		t.Fatalf("heartbeat machine: %v", err)
	}
	var sessions atomic.Uint64
	claimer, err := hubclient.NewWorkspaceClaimer(native, hubclient.WorkspaceLaneConfig{
		PolicyID: policy.ID, MachineID: f.runner.binding.MachineID,
		SessionID: func() (string, error) { return fmt.Sprintf("lane-session-%d", sessions.Add(1)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.source = laneSourceRepository(t)
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		Root: filepath.Join(laneShortTempDir(t), "worktrees"), SourceRoot: f.source, AutoBranch: true, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.lane, err = workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer: claimer, Hub: native, Logger: discardLogger(), Poll: 50 * time.Millisecond,
		Worktree: &workspacerunner.GitWorktree{Backend: backend, ProjectID: string(f.project.ID), Resolve: claimer.RunIdentifier},
		Support:  support, Shell: "/bin/sh", Hostname: "runner-host",
		Reporter: laneActionReporter{native: native},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.lane.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the workspace lane did not drain")
		}
	})
	return f
}

func laneShortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lane")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func laneSourceRepository(t *testing.T) string {
	t.Helper()
	root := laneShortTempDir(t)
	laneGit(t, root, "init", "-q", ".")
	laneGit(t, root, "config", "core.longpaths", "true")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Lane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	laneGit(t, root, "add", "-A")
	laneGit(t, root, "commit", "-qm", "first")
	return root
}

func laneGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{
		"-c", "init.defaultBranch=main", "-c", "user.name=Fixture",
		"-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false",
	}, args...)...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *workspaceLaneFixture) open(t *testing.T, subject string, requires []string) string {
	t.Helper()
	issue := f.create(t, subject)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces", f.token, map[string]any{
		"idempotency_key": "lane-" + string(issue.WorkItemID), "work_item_id": string(issue.WorkItemID), "requires": requires,
	})
	requireNativeStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	return session.ID
}

func (f *workspaceLaneFixture) read(t *testing.T, id string) workspacesession.Session {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/workspaces/"+id, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var session workspacesession.Session
	decodeHubResponse(t, response, &session)
	return session
}

func (f *workspaceLaneFixture) awaitReady(t *testing.T, id string) workspacesession.Session {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		session := f.read(t, id)
		if session.State == workspacesession.StateReady {
			// Ready describes the worktree; the runner attaches its relay
			// afterward. Lifecycle tests need both before sending frames.
			relay := f.service.workspaces.relay
			relay.mu.Lock()
			room := relay.rooms[id]
			attached := room != nil && room.runner != nil
			relay.mu.Unlock()
			if attached {
				return session
			}
		}
		if workspacesession.Terminal(session.State) {
			t.Fatalf("workspace ended before ready: %+v", session)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the runner never made the workspace ready with an attached relay")
	return workspacesession.Session{}
}

func (f *workspaceLaneFixture) awaitLaneIdle(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for f.lane.Open() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the lane still holds %d workspaces", f.lane.Open())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *workspaceLaneFixture) dialPerson(t *testing.T, id string) *relayClient {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/workspaces/"+id+"/relay-tickets", f.token, map[string]any{})
	requireNativeStatus(t, response, http.StatusCreated)
	var minted workspaceRelayTicketResponse
	decodeHubResponse(t, response, &minted)
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") + f.base + "/workspaces/" + id + "/relay?ticket=" + minted.Ticket
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	socket, upgrade, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + f.token}}})
	closeRelayDial(upgrade)
	if err != nil {
		t.Fatalf("dial person relay: %v", err)
	}
	socket.SetReadLimit(relayReadLimit)
	t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "test over") })
	return &relayClient{t: t, socket: socket}
}

func receiveType(t *testing.T, client *relayClient, kind string) workspacesession.Frame {
	t.Helper()
	for range 64 {
		frame := client.receive()
		if frame.Type == kind {
			return frame
		}
		if frame.Type == workspacesession.TypeError {
			t.Fatalf("waiting for %s, got error %s", kind, frame.Payload)
		}
	}
	t.Fatalf("never received a %s frame", kind)
	return workspacesession.Frame{}
}

func readCounter(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return -1
	}
	return value
}

func awaitCounter(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for readCounter(t, path) <= 0 {
		if time.Now().After(deadline) {
			t.Fatal("the shell never started writing its counter")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func requireCounterStopped(t *testing.T, path string) {
	t.Helper()
	before := readCounter(t, path)
	time.Sleep(time.Second)
	if after := readCounter(t, path); after != before {
		t.Fatalf("the shell is still running after the session ended: counter %d -> %d", before, after)
	}
}

func terminalAvailable() bool {
	if !workspaceterminal.Supported {
		return false
	}
	_, err := os.Stat("/bin/sh")
	return err == nil
}

// startCounter opens a terminal through the relay and leaves a shell loop
// writing an increasing counter outside the worktree, so the test can tell
// whether the PTY outlived the session.
func startCounter(t *testing.T, person *relayClient) string {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "counter")
	person.send(terminalOpenFrame())
	opened := receiveType(t, person, workspacesession.TypeTerminalOpened)
	command := fmt.Sprintf("i=0; while :; do i=$((i+1)); echo $i > %s; sleep 0.1; done\n", counter)
	person.send(terminalFrameOf(t, workspacesession.TypeTerminalInput, opened.Stream, workspacesession.TerminalInput{Data: command}))
	awaitCounter(t, counter)
	return counter
}

func TestWorkspaceLaneServesAHostedWorkspaceUntilItsLeaseIsLost(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	f := newWorkspaceLaneFixture(t)
	id := f.open(t, "lease-subject", []string{workspacesession.CapabilityFiles, workspacesession.CapabilityGit})
	ready := f.awaitReady(t, id)
	if ready.WorktreePath == "" || ready.MachineHostname != "runner-host" {
		t.Fatalf("ready workspace = %+v, want the runner's worktree path and host", ready)
	}
	if ready.Capabilities == nil || !ready.Capabilities.Files || !ready.Capabilities.Git || !ready.Capabilities.Exec {
		t.Fatalf("ready capabilities = %+v, want files, git and exec", ready.Capabilities)
	}
	if f.lane.Open() != 1 {
		t.Fatalf("lane holds %d workspaces, want 1", f.lane.Open())
	}

	person := f.dialPerson(t, id)
	person.send(filesListFrame(""))
	listed := receiveType(t, person, workspacesession.TypeFilesListed)
	var files workspacesession.FilesListed
	if err := json.Unmarshal(listed.Payload, &files); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range files.Entries {
		found = found || entry.Name == "README.md"
	}
	if !found {
		t.Fatalf("files listing = %+v, want README.md from the worktree", files.Entries)
	}

	person.send(gitFrame(workspacesession.TypeGitStatus, ""))
	statusFrame := receiveType(t, person, workspacesession.TypeGitStatus)
	var status workspacesession.GitStatus
	if err := json.Unmarshal(statusFrame.Payload, &status); err != nil {
		t.Fatal(err)
	}
	if head := laneGit(t, f.source, "rev-parse", "HEAD"); status.HeadSHA != head {
		t.Fatalf("git status head = %q, want %q", status.HeadSHA, head)
	}

	var counter string
	if terminalAvailable() {
		counter = startCounter(t, person)
	}

	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE leases SET expires_at = ? WHERE released_at IS NULL", formatHubTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	f.awaitLaneIdle(t, workspacesession.HeartbeatInterval+30*time.Second)
	if counter != "" {
		requireCounterStopped(t, counter)
	}
	if _, err := os.Stat(ready.WorktreePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session worktree %s remains after lease loss: %v", ready.WorktreePath, err)
	}
}

func TestWorkspaceLaneUnbindsWhenTheWorkspaceIsClosed(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	f := newWorkspaceLaneFixture(t)
	id := f.open(t, "close-subject", []string{workspacesession.CapabilityFiles})
	ready := f.awaitReady(t, id)
	person := f.dialPerson(t, id)
	var counter string
	if terminalAvailable() {
		counter = startCounter(t, person)
	}

	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+id, f.token, nil), http.StatusNoContent)
	f.awaitLaneIdle(t, 30*time.Second)
	if counter != "" {
		requireCounterStopped(t, counter)
	}
	if closed := f.read(t, id); closed.State != workspacesession.StateClosed {
		t.Fatalf("workspace = %+v, want closed", closed)
	}
	if _, err := os.Stat(ready.WorktreePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session worktree %s remains after close: %v", ready.WorktreePath, err)
	}
	var open int
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM leases WHERE released_at IS NULL AND machine_id = ?", string(f.runner.binding.MachineID)).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d leases remain held after the workspace closed", open)
	}
}

func TestNativeCapabilitiesAdvertiseWorkspaceSessionsOnlyWhenServed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		workspace *WorkspaceConfig
		want      bool
	}{
		{name: "workspaces enabled", workspace: &WorkspaceConfig{Enabled: true}, want: true},
		{name: "workspaces disabled", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Workspace: test.workspace})
			f := newNativeFixture(t, service, "", "capabilities")
			response := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/capabilities", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var document struct {
				Features []string `json:"features"`
			}
			decodeHubResponse(t, response, &document)
			if got := slices.Contains(document.Features, tracker.NativeWorkspaceCapability); got != test.want {
				t.Fatalf("features = %v, advertise workspace sessions = %t, want %t", document.Features, got, test.want)
			}
		})
	}
}

// TestWorkspaceLaneClaimsWhatTheClientOpens opens workspaces with the requires
// lists web/conversation sends (RightPanel's WORKSPACE_REQUIRES, headerGit's
// GIT_REQUIRES) and with none at all, and expects this runner to claim each.
func TestWorkspaceLaneClaimsWhatTheClientOpens(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	f := newWorkspaceLaneFixture(t)
	tests := []struct {
		name     string
		requires []string
	}{
		{name: "files panel", requires: []string{"files", "exec"}},
		{name: "header git", requires: []string{"files", "git"}},
		{name: "no requires", requires: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := f.open(t, "client "+test.name, test.requires)
			ready := f.awaitReady(t, id)
			if !ready.Capabilities.Satisfies(ready.Requires) {
				t.Fatalf("ready workspace requires %v beyond capabilities %+v", ready.Requires, ready.Capabilities)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, f.base+"/workspaces/"+id, f.token, nil), http.StatusNoContent)
			f.awaitLaneIdle(t, 30*time.Second)
		})
	}
}

func (f *workspaceLaneFixture) action(t *testing.T, body map[string]any) workspacesession.Action {
	t.Helper()
	body["idempotency_key"] = newNativeID("actkey")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions", f.token, body)
	requireNativeStatus(t, response, http.StatusCreated)
	var authored workspacesession.Action
	decodeHubResponse(t, response, &authored)
	return authored
}

func (f *workspaceLaneFixture) awaitRun(t *testing.T, action workspacesession.Action, dispatch bool) workspacesession.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if dispatch {
			f.service.workspaces.sweep(t.Context())
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/actions/"+action.ID+"/runs", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var runs projectActionRunList
		decodeHubResponse(t, response, &runs)
		if len(runs.Items) == 1 && runs.Items[0].Status != workspacesession.RunQueued && runs.Items[0].Status != workspacesession.RunRunning {
			return runs.Items[0]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("action %s never finished a run", action.Name)
	return workspacesession.Run{}
}

// TestWorkspaceLaneRunsProjectActions covers both ways a runner starts a
// project action: the run-on-worktree-creation set it runs before reporting
// ready, and a run queued through the API that the hub hands it over the relay.
func TestWorkspaceLaneRunsProjectActions(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	if !terminalAvailable() {
		t.Skip("the action commands are POSIX shell syntax")
	}
	f := newWorkspaceLaneFixture(t)
	setup := f.action(t, map[string]any{"name": "Setup", "command": "echo setup > setup-ran", "run_on_worktree_creation": true})
	id := f.open(t, "actions-subject", []string{"files", "exec"})
	ready := f.awaitReady(t, id)
	if run := f.awaitRun(t, setup, false); run.Status != workspacesession.RunSucceeded || run.WorkspaceID != id {
		t.Fatalf("setup run = %+v, want a succeeded run on %s", run, id)
	}
	if _, err := os.Stat(filepath.Join(ready.WorktreePath, "setup-ran")); err != nil {
		t.Fatalf("the setup action did not run in the worktree: %v", err)
	}

	build := f.action(t, map[string]any{"name": "Build", "command": "echo built > build-ran"})
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/actions/"+build.ID+"/runs", f.token,
		map[string]any{"idempotency_key": newNativeID("runkey"), "workspace_id": id})
	requireNativeStatus(t, response, http.StatusAccepted)
	time.Sleep(actionRunDispatchGrace + 100*time.Millisecond)
	if run := f.awaitRun(t, build, true); run.Status != workspacesession.RunSucceeded {
		t.Fatalf("dispatched run = %+v, want succeeded", run)
	}
	if _, err := os.Stat(filepath.Join(ready.WorktreePath, "build-ran")); err != nil {
		t.Fatalf("the dispatched action did not run in the worktree: %v", err)
	}
}

func TestRelayedRequiresDropsOnlyUnservedSurfaces(t *testing.T) {
	t.Parallel()
	tests := []struct {
		requires []string
		want     []string
	}{
		{requires: []string{"files", "exec"}, want: []string{"files", "exec"}},
		{requires: []string{"files", "diff"}, want: []string{"files"}},
		{requires: []string{"terminal", "preview", "git"}, want: []string{"terminal", "git"}},
	}
	for _, test := range tests {
		if got := relayedRequires(test.requires); !slices.Equal(got, test.want) {
			t.Errorf("relayedRequires(%v) = %v, want %v", test.requires, got, test.want)
		}
	}
}

type laneActionReporter struct {
	native *hubclient.NativeClient
}

func (r laneActionReporter) ReportActionRun(ctx context.Context, workspaceID string, identity hubclient.WorkspaceIdentity, run workspacerunner.ActionRun) (workspacesession.Run, error) {
	return r.native.ReportWorkspaceActionRun(ctx, workspaceID, hubclient.WorkspaceActionRunReport{
		WorkspaceIdentity: identity, ActionID: run.ActionID, RunID: run.RunID, Status: run.Status,
		ExitCode: run.ExitCode, Reason: run.Reason, StartedAt: run.StartedAt,
		FinishedAt: run.FinishedAt, Output: run.Output, Truncated: run.Truncated,
	})
}
