package runnerruntime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacehub"
	"github.com/digitaldrywood/detent/internal/hubserver/workspacerelay"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

type workspaceLaneFixture struct {
	hubfixture.Native
	server *httptest.Server
	runner hubtest.SeededRunner
	lane   *workspacerunner.Lane
	relay  *workspaceLaneRelay
	source string
}

func newWorkspaceLaneFixture(t *testing.T) *workspaceLaneFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	f := &workspaceLaneFixture{Native: hubfixture.NewDefaultNative(t, hubfixture.Config{
		Workspace: &workspacehub.Config{Enabled: true, PersonMaxOpen: 10, PlanMaxOpen: 10,
			Terminal: workspacehub.TerminalConfig{Enabled: true}},
	})}
	policy := hubfixture.Policy()
	f.server = httptest.NewServer(f.Hub.Handler())
	t.Cleanup(f.server.Close)

	f.runner = hubtest.Runner(t, f.Hub.Repository, f.Organization(), hubtest.RunnerOptions{
		Projects: []tracker.ProjectID{f.Project.ID}, Hostname: "runner-host", Capacity: 4, Now: f.Hub.Now(),
	})
	client, err := hubclient.New(hubclient.Config{
		URL: f.server.URL, HTTPClient: f.server.Client(),
		TokenSource: func() string { return f.runner.Credential.Token },
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.Project.OrganizationID, f.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	support := workspacerunner.Support{Terminal: true}
	if err := native.HeartbeatMachine(t.Context(), hubclient.Machine{
		ID: f.runner.Identity.MachineID, Hostname: "runner-host", DisplayName: "runner", Capacity: 4, Version: "test",
		BackendIsolation:      isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}},
		CapacityConfig:        &runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 4, ClientLimit: 4, RuntimeLimit: 4, Manageable: true, ObservedAt: f.Hub.Now()},
		WorkspaceCapabilities: workspacerunner.Capabilities(support), WorkspaceIsolation: workspacesession.IsolationUser,
	}); err != nil {
		t.Fatalf("heartbeat machine: %v", err)
	}
	var sessions atomic.Uint64
	claimer, err := hubclient.NewWorkspaceClaimer(native, hubclient.WorkspaceLaneConfig{
		PolicyID: policy.ID, MachineID: f.runner.Identity.MachineID,
		SessionID: func() (string, error) { return fmt.Sprintf("lane-session-%d", sessions.Add(1)), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.source = laneSourceRepository(t)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		Root: filepath.Join(laneShortTempDir(t), "worktrees"), SourceRoot: f.source, AutoBranch: true, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.relay = &workspaceLaneRelay{Hub: native}
	f.lane, err = workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer: claimer, Hub: f.relay, Logger: logger, Poll: 50 * time.Millisecond,
		Worktree: &workspacerunner.GitWorktree{Backend: backend, ProjectID: string(f.Project.ID), Resolve: claimer.RunIdentifier},
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
		<-done
		f.relay.probes.Wait()
	})
	return f
}

type workspaceLaneRelay struct {
	workspacerunner.Hub
	probes sync.WaitGroup
	ready  sync.Map
}

func (r *workspaceLaneRelay) DialWorkspaceRelay(ctx context.Context, id string, identity hubclient.WorkspaceIdentity) (*websocket.Conn, error) {
	socket, err := r.Hub.DialWorkspaceRelay(ctx, id, identity)
	if err != nil {
		return nil, err
	}
	r.probes.Go(func() {
		if socket.Ping(ctx) == nil {
			r.ready.Store(id, struct{}{})
		}
	})
	return socket, nil
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
	issue := f.Create(t, subject)
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/workspaces", f.Token, map[string]any{
		"idempotency_key": "lane-" + string(issue.WorkItemID), "work_item_id": string(issue.WorkItemID), "requires": requires,
	})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var session workspacesession.Session
	hubfixture.Decode(t, response, &session)
	return session.ID
}

func (f *workspaceLaneFixture) read(t *testing.T, id string) workspacesession.Session {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/workspaces/"+id, f.Token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var session workspacesession.Session
	hubfixture.Decode(t, response, &session)
	return session
}

func (f *workspaceLaneFixture) awaitReady(t *testing.T, id string) workspacesession.Session {
	t.Helper()
	var ready workspacesession.Session
	testenv.WaitUntil(t, t.Context(), func() error {
		session := f.read(t, id)
		if session.State == workspacesession.StateReady {
			if _, ok := f.relay.ready.Load(id); ok {
				ready = session
				return nil
			}
		}
		if workspacesession.Terminal(session.State) {
			t.Fatalf("workspace ended before ready: %+v", session)
		}
		return errors.New("the runner never made the workspace ready with an attached relay")
	})
	return ready
}

func (f *workspaceLaneFixture) awaitLaneIdle(t *testing.T) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		if open := f.lane.Open(); open != 0 {
			return fmt.Errorf("the lane still holds %d workspaces", open)
		}
		return nil
	})
}

func (f *workspaceLaneFixture) dialPerson(t *testing.T, id string) *workspaceRelayClient {
	t.Helper()
	return dialWorkspacePerson(t, f.Native, f.server, id)
}

func dialWorkspacePerson(t *testing.T, project hubfixture.Native, server *httptest.Server, id string) *workspaceRelayClient {
	t.Helper()
	response := project.Request(t, http.MethodPost, project.Base+"/workspaces/"+id+"/relay-tickets", map[string]any{})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var minted workspacerelay.TicketResponse
	hubfixture.Decode(t, response, &minted)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + project.Base + "/workspaces/" + id + "/relay?ticket=" + minted.Ticket
	return dialWorkspaceSocket(t, url, http.Header{"Authorization": {"Bearer " + project.Token}})
}

func dialWorkspaceSocket(t *testing.T, url string, headers http.Header) *workspaceRelayClient {
	t.Helper()
	socket, upgrade, err := websocket.Dial(testenv.WaitContext(t, t.Context()), url, &websocket.DialOptions{HTTPHeader: headers})
	if upgrade != nil && upgrade.Body != nil {
		_ = upgrade.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	socket.SetReadLimit(workspacerelay.ReadLimit)
	t.Cleanup(func() { _ = socket.Close(websocket.StatusNormalClosure, "test over") })
	return &workspaceRelayClient{t: t, socket: socket}
}

func laneFrame(t *testing.T, channel, kind, stream string, payload any) workspacesession.Frame {
	t.Helper()
	encoded, err := workspacesession.Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{Channel: channel, Type: kind, Stream: stream, Payload: encoded}
}

type workspaceRelayClient struct {
	t       *testing.T
	socket  *websocket.Conn
	pending <-chan workspaceRelayMessage
}

type workspaceRelayMessage struct {
	kind websocket.MessageType
	data []byte
	err  error
}

func (c *workspaceRelayClient) read(ctx context.Context) workspaceRelayMessage {
	kind, data, err := c.socket.Read(ctx)
	return workspaceRelayMessage{kind: kind, data: data, err: err}
}

func (c *workspaceRelayClient) awaitServing() {
	c.t.Helper()
	ctx := testenv.WaitContext(c.t, c.t.Context())
	read := make(chan workspaceRelayMessage, 1)
	done := make(chan struct{})
	c.t.Cleanup(func() { <-done })
	go func() {
		defer close(done)
		read <- c.read(ctx)
	}()
	c.pending = read
	if err := c.socket.Ping(ctx); err != nil {
		c.t.Fatalf("relay did not start reading frames: %v", err)
	}
}

func (c *workspaceRelayClient) send(frame workspacesession.Frame) {
	c.t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx := testenv.WaitContext(c.t, c.t.Context())
	if err := c.socket.Write(ctx, websocket.MessageText, encoded); err != nil {
		c.t.Fatalf("write frame: %v", err)
	}
}

func (c *workspaceRelayClient) receive() workspacesession.Frame {
	c.t.Helper()
	v := workspaceRelayMessage{}
	if c.pending == nil {
		v = c.read(testenv.WaitContext(c.t, c.t.Context()))
	} else {
		v = <-c.pending
		c.pending = nil
	}
	if v.err != nil {
		c.t.Fatalf("read frame: %v", v.err)
	}
	if v.kind != websocket.MessageText {
		c.t.Fatalf("frame message type = %v, want text", v.kind)
	}
	var frame workspacesession.Frame
	if err := json.Unmarshal(v.data, &frame); err != nil {
		c.t.Fatalf("decode frame %q: %v", v.data, err)
	}
	return frame
}

func receiveType(t *testing.T, client *workspaceRelayClient, kind string) workspacesession.Frame {
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

func terminalAvailable() bool {
	if !workspaceterminal.Supported {
		return false
	}
	_, err := os.Stat("/bin/sh")
	return err == nil
}

func startWorkspaceShell(t *testing.T, person *workspaceRelayClient) *os.Process {
	t.Helper()
	person.send(laneFrame(t, workspacesession.ChannelTerminal, workspacesession.TypeOpen, "", workspacesession.TerminalOpen{Cols: 80, Rows: 24}))
	opened := receiveType(t, person, workspacesession.TypeTerminalOpened)
	var shell workspacesession.TerminalOpened
	if err := json.Unmarshal(opened.Payload, &shell); err != nil {
		t.Fatal(err)
	}
	if shell.PID <= 0 {
		t.Fatalf("terminal opened without a shell process: %+v", shell)
	}
	process, err := os.FindProcess(shell.PID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Release() })
	if err := process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("terminal shell was not running: %v", err)
	}
	ready := filepath.Join(t.TempDir(), "terminal-ready")
	quoted := "'" + strings.ReplaceAll(ready, "'", "'\\''") + "'"
	command := "printf ready > " + quoted + "\n"
	person.send(laneFrame(t, workspacesession.ChannelTerminal, workspacesession.TypeTerminalInput, opened.Stream, workspacesession.TerminalInput{Data: command}))
	testenv.WaitUntil(t, t.Context(), func() error {
		contents, err := os.ReadFile(ready)
		if err != nil {
			return err
		}
		if string(contents) != "ready" {
			return errors.New("terminal shell has not executed its input")
		}
		return nil
	})
	return process
}

func requireWorkspaceShellExited(t *testing.T, process *os.Process) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		err := process.Signal(syscall.Signal(0))
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("check shell process %d: %w", process.Pid, err)
		}
		return fmt.Errorf("terminal shell process %d is still running", process.Pid)
	})
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

	person := f.dialPerson(t, id)
	person.send(laneFrame(t, workspacesession.ChannelFiles, workspacesession.TypeFilesList, "", workspacesession.FilesRequest{}))
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

	person.send(laneFrame(t, workspacesession.ChannelGit, workspacesession.TypeGitStatus, "", workspacesession.GitRequest{}))
	statusFrame := receiveType(t, person, workspacesession.TypeGitStatus)
	var status workspacesession.GitStatus
	if err := json.Unmarshal(statusFrame.Payload, &status); err != nil {
		t.Fatal(err)
	}
	if head := laneGit(t, f.source, "rev-parse", "HEAD"); status.HeadSHA != head {
		t.Fatalf("git status head = %q, want %q", status.HeadSHA, head)
	}

	var shell *os.Process
	if terminalAvailable() {
		shell = startWorkspaceShell(t, person)
	}

	result, err := f.Hub.Admin.ExecContext(t.Context(),
		"UPDATE leases SET acquired_at=acquired_at+($1::timestamptz-expires_at),renewed_at=renewed_at+($1::timestamptz-expires_at),expires_at=$1 WHERE organization_id=$2 AND machine_id=$3 AND released_at IS NULL", time.Now().Add(-time.Minute), f.Project.OrganizationID, f.runner.Identity.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("expired leases = %d, error=%v", count, err)
	}
	f.awaitLaneIdle(t)
	if shell != nil {
		requireWorkspaceShellExited(t, shell)
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
	var shell *os.Process
	if terminalAvailable() {
		shell = startWorkspaceShell(t, person)
	}

	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, f.Base+"/workspaces/"+id, f.Token, nil), http.StatusNoContent)
	f.awaitLaneIdle(t)
	if shell != nil {
		requireWorkspaceShellExited(t, shell)
	}
	if closed := f.read(t, id); closed.State != workspacesession.StateClosed {
		t.Fatalf("workspace = %+v, want closed", closed)
	}
	if _, err := os.Stat(ready.WorktreePath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the session worktree %s remains after close: %v", ready.WorktreePath, err)
	}
	var open int
	if err := f.Hub.Admin.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM leases WHERE organization_id=$1 AND released_at IS NULL AND machine_id=$2", f.Project.OrganizationID, f.runner.Identity.MachineID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatalf("%d leases remain held after the workspace closed", open)
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
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodDelete, f.Base+"/workspaces/"+id, f.Token, nil), http.StatusNoContent)
			f.awaitLaneIdle(t)
		})
	}
}

func (f *workspaceLaneFixture) action(t *testing.T, body map[string]any) workspacesession.Action {
	t.Helper()
	body["idempotency_key"] = hubapi.NewID("actkey")
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/actions", f.Token, body)
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var authored workspacesession.Action
	hubfixture.Decode(t, response, &authored)
	return authored
}

func (f *workspaceLaneFixture) awaitRun(t *testing.T, action workspacesession.Action) workspacesession.Run {
	t.Helper()
	var finished workspacesession.Run
	testenv.WaitUntil(t, t.Context(), func() error {
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/actions/"+action.ID+"/runs", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var runs struct {
			Items []workspacesession.Run `json:"items"`
		}
		hubfixture.Decode(t, response, &runs)
		if len(runs.Items) == 1 && runs.Items[0].Status != workspacesession.RunQueued && runs.Items[0].Status != workspacesession.RunRunning {
			finished = runs.Items[0]
			return nil
		}
		return fmt.Errorf("action %s never finished a run", action.Name)
	})
	return finished
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
	if run := f.awaitRun(t, setup); run.Status != workspacesession.RunSucceeded || run.WorkspaceID != id {
		t.Fatalf("setup run = %+v, want a succeeded run on %s", run, id)
	}
	if _, err := os.Stat(filepath.Join(ready.WorktreePath, "setup-ran")); err != nil {
		t.Fatalf("the setup action did not run in the worktree: %v", err)
	}

	build := f.action(t, map[string]any{"name": "Build", "command": "echo built > build-ran"})
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/actions/"+build.ID+"/runs", f.Token,
		map[string]any{"idempotency_key": hubapi.NewID("runkey"), "workspace_id": id})
	hubfixture.RequireStatus(t, response, http.StatusAccepted)
	if run := f.awaitRun(t, build); run.Status != workspacesession.RunSucceeded {
		t.Fatalf("dispatched run = %+v, want succeeded", run)
	}
	if _, err := os.Stat(filepath.Join(ready.WorktreePath, "build-ran")); err != nil {
		t.Fatalf("the dispatched action did not run in the worktree: %v", err)
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
