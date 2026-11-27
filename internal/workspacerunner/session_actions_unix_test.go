//go:build !windows

package workspacerunner_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestSessionActionIsolation(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name      string
		isolation string
		wantExec  bool
	}
	tests := []scenario{
		{name: "user isolation disables both action paths", isolation: workspacesession.IsolationUser},
	}
	if runtime.GOOS == "darwin" {
		tests = append(tests, scenario{name: "confines both action paths to the worktree", isolation: workspacesession.IsolationSandbox, wantExec: true})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "worktree")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			reporter := &recordingReporter{}
			command := "printf ran > creation.txt; ! cat ../outside.txt >/dev/null 2>&1"
			f := startSessionOver(t, root, creationCheckout(workspacesession.WorktreeFresh), []workspacesession.Action{
				{ID: "act_setup", Name: "Setup", Command: command, RunOnWorktreeCreation: true},
			}, func(config *workspacerunner.Config) {
				config.Shell = "/bin/sh"
				config.Support = workspacerunner.Support{RequireSandbox: true, Isolation: test.isolation}
				config.Reporter = reporter
			})
			reported := reporter.snapshot()
			created, err := os.ReadFile(filepath.Join(root, "creation.txt"))
			f.send(t, execRunFrame(t, "conn:1", "test \"$(cat creation.txt)\" = ran && ! cat ../outside.txt >/dev/null 2>&1 && printf confined"))
			if !test.wantExec {
				if len(reported) != 0 || !errors.Is(err, os.ErrNotExist) {
					t.Fatal("creation action ran without a sandbox")
				}
				frame := f.receive(t)
				var payload workspacesession.ErrorPayload
				if err := json.Unmarshal(frame.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if frame.Type != workspacesession.TypeError || payload.Code != workspacesession.CodeUnsupported {
					t.Fatalf("manual action response = %q / %q, want unsupported", frame.Type, payload.Code)
				}
				return
			}
			if err != nil || string(created) != "ran" || len(reported) != 2 || reported[1].Status != workspacesession.RunSucceeded {
				t.Fatalf("creation action did not complete inside the sandbox: %v, %v", err, reported)
			}
			frames := f.receiveUntil(t, workspacesession.TypeClosed)
			if got := execOutput(t, frames); !strings.HasSuffix(got, "confined") || execExited(t, frames).Code != 0 {
				t.Fatalf("manual action did not complete inside the sandbox: %q", got)
			}
		})
	}
}

func TestSessionRunsAnActionOnTheExecChannel(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "conn:1", "printf ready; printf oops >&2"))
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if len(frames) < 3 {
		t.Fatalf("a run answered %d frames, want output, exited and closed", len(frames))
	}
	// One ordered stream: stderr is interleaved with stdout the way the shell
	// interleaved it, so a reader sees what a terminal would have shown.
	if got := execOutput(t, frames); !strings.Contains(got, "ready") || !strings.Contains(got, "oops") {
		t.Fatalf("output = %q, want both streams", got)
	}
	if exited := execExited(t, frames); exited.Code != 0 || exited.Signal != "" {
		t.Fatalf("exited = %+v, want a clean exit", exited)
	}
	// The hub releases a stream's slot on close or closed and never on exited,
	// so a run that stopped at exited would leak one slot each time until the
	// workspace refused the next run.
	last := frames[len(frames)-1]
	if last.Type != workspacesession.TypeClosed || last.Stream != "conn:1" {
		t.Fatalf("the last frame was %+v, want closed on the run's own stream", last)
	}
	for index, frame := range frames {
		if frame.Channel != workspacesession.ChannelExec || frame.Stream != "conn:1" {
			t.Fatalf("frame %d = %+v, want it on the exec channel's own stream", index, frame)
		}
	}
}

// TestSessionRefusesARunAfterLeaseLoss is the files path's rule applied to the
// one thing on this surface that outlives the frame that asked for it: the
// lease is validated immediately before the process starts, so a run under a
// lost lease never becomes a process at all.
func TestSessionRefusesARunAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		config.Now = func() time.Time { return expired }
	})
	witness := filepath.Join(f.worktree.path, "witness")
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, execRunFrame(t, "conn:1", "printf ran > "+witness))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a run after lease loss answered %+v, want stale_execution", payload)
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the command never to have run", err)
	}
}

// TestSessionStopsARunWhenTheStreamIsClosed covers the process the person
// stopped: the close ends their stream and the whole process group goes with
// it, not the shell alone -- the grandchild here is what an installer or a
// build server would be.
func TestSessionStopsARunWhenTheStreamIsClosed(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)
	witness := filepath.Join(f.worktree.path, "witness")

	f.send(t, execRunFrame(t, "conn:1", "printf started; sh -c 'sleep 2; : > "+witness+"' & sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	f.send(t, workspacesession.Frame{
		Channel: workspacesession.ChannelExec, Stream: "conn:1", Type: workspacesession.TypeClose,
	})
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if exited := execExited(t, frames); exited.Code == 0 {
		t.Fatalf("exited = %+v, want a killed run to say so", exited)
	}

	// Past the grandchild's own sleep: if the group had survived the close, the
	// witness would be there by now.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the process group to have died with the stream", err)
	}
}

func TestSessionRefusesASecondRunOnAStream(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "conn:1", "printf started; sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	f.send(t, execRunFrame(t, "conn:1", "printf second"))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	// One stream is one run: two runs interleaved on one stream would leave the
	// person with a single exited frame to explain both of them.
	if answer.Type != workspacesession.TypeError || payload.Code != workspacesession.CodeInvalidFrame {
		t.Fatalf("a second run answered %+v / %+v, want invalid_frame", answer, payload)
	}
}

func TestSessionRunsAnActionTheHubDispatched(t *testing.T) {
	t.Parallel()
	f := startSession(t, nil)

	f.send(t, execRunFrame(t, "relayhub:1", "printf ready; printf oops >&2"))
	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if got := execOutput(t, frames); !strings.Contains(got, "ready") || !strings.Contains(got, "oops") {
		t.Fatalf("output = %q, want both streams of a hub-dispatched run", got)
	}
	if exited := execExited(t, frames); exited.Code != 0 || exited.Signal != "" {
		t.Fatalf("exited = %+v, want a clean exit", exited)
	}
	// The stream's slot goes back on closed and never on exited, exactly as it
	// does for a person's run: the hub counts its own streams against the
	// workspace's budget, so a leaked one would refuse the next dispatch.
	last := frames[len(frames)-1]
	if last.Type != workspacesession.TypeClosed || last.Stream != "relayhub:1" {
		t.Fatalf("the last frame was %+v, want closed on the hub's own stream", last)
	}
}

// TestSessionRefusesAHubDispatchedRunAfterLeaseLoss is the lease rule on the
// path that has no person behind it. A dispatched run is still a command in a
// worktree, so it is validated immediately before the process starts and never
// executed under a lease this runner no longer holds.
func TestSessionRefusesAHubDispatchedRunAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	expired := time.Now()
	f := startSession(t, func(config *workspacerunner.Config) {
		config.Now = func() time.Time { return expired }
	})
	witness := filepath.Join(f.worktree.path, "witness")
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, execRunFrame(t, "relayhub:1", "printf ran > "+witness))
	answer := f.receive(t)
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a dispatched run after lease loss answered %+v, want stale_execution", payload)
	}
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the command never to have run", err)
	}
}

// TestSessionStopsAHubDispatchedRunWhenTheLeaseIsLost covers the half a
// pre-start check cannot: a run outlives the frame that asked for it, and a
// dispatched run has no person whose closing tab would end it. The lease is
// re-checked while the process runs, and the whole process group goes when it
// fails -- the grandchild here is what a build server would be.
func TestSessionStopsAHubDispatchedRunWhenTheLeaseIsLost(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	now := time.Now()
	// The heartbeat is pushed out of the way rather than left at a second: a
	// successful heartbeat renews the lease from the runner's own clock, so a
	// loop ticking beside the clock this test moves would keep handing the
	// lease back and the run would never be stopped by anything.
	f := startSessionWith(t, worktreeWith(t), func(checkout *hubclient.WorkspaceCheckout) {
		checkout.HeartbeatSeconds = 3600
	}, func(config *workspacerunner.Config) {
		config.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})
	witness := filepath.Join(f.worktree.path, "witness")

	f.send(t, execRunFrame(t, "relayhub:1", "printf started; sh -c 'sleep 3; : > "+witness+"' & sleep 30"))
	if frame := f.receive(t); frame.Type != workspacesession.TypeExecOutput {
		t.Fatalf("the run's first frame was %+v, want output", frame)
	}
	mu.Lock()
	now = now.Add(workspacesession.LeaseTTL + time.Minute)
	mu.Unlock()

	frames := f.receiveUntil(t, workspacesession.TypeClosed)
	if exited := execExited(t, frames); exited.Code == 0 {
		t.Fatalf("exited = %+v, want a killed run to say so", exited)
	}
	// Past the grandchild's own sleep: if the group had survived the lease
	// loss, the witness would be there by now and a worktree this runner no
	// longer owns would still be being written to.
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(witness); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat witness = %v, want the process group to have died with the lease", err)
	}
}

// TestSessionRunsFreshWorktreeActionsInOrder is why the set is run one at a
// time: the order is the author's meaning, and an author who wants install
// before build writes install first.
func TestSessionRunsFreshWorktreeActionsInOrder(t *testing.T) {
	t.Parallel()
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeFresh), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf install >> actions.log", RunOnWorktreeCreation: true},
		{ID: "act_build", Name: "Build", Command: "printf build >> actions.log", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	logged, err := os.ReadFile(filepath.Join(f.worktree.path, "actions.log"))
	if err != nil {
		t.Fatalf("read the actions' own log: %v", err)
	}
	if string(logged) != "installbuild" {
		t.Fatalf("the actions wrote %q, want them run in authoring order", logged)
	}
	reports := reporter.snapshot()
	want := []struct {
		action string
		status string
	}{
		{action: "act_install", status: workspacesession.RunRunning},
		{action: "act_install", status: workspacesession.RunSucceeded},
		{action: "act_build", status: workspacesession.RunRunning},
		{action: "act_build", status: workspacesession.RunSucceeded},
	}
	if len(reports) != len(want) {
		t.Fatalf("reports = %+v, want %d of them", reports, len(want))
	}
	for index, expected := range want {
		if reports[index].ActionID != expected.action || reports[index].Status != expected.status {
			t.Fatalf("report %d = %+v, want %s %s", index, reports[index], expected.action, expected.status)
		}
	}
	// A run's first report carries no id, because the hub creates the row and
	// answers with one; every later report of that run carries it back.
	if reports[0].RunID != "" || reports[1].RunID != "run_1" || reports[3].RunID != "run_2" {
		t.Fatalf("run ids = %q, %q, %q; want the hub's own ids threaded through",
			reports[0].RunID, reports[1].RunID, reports[3].RunID)
	}
	if reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
		t.Fatalf("exit code = %v, want the command's own 0", reports[1].ExitCode)
	}
}

// TestSessionRunsNoActionsForARetainedWorktree is the other half of the rule:
// the setup already ran when this worktree was created, and running it again
// would redo that work on a tree someone may be reading.
func TestSessionRunsNoActionsForARetainedWorktree(t *testing.T) {
	t.Parallel()
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeRetained), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf install >> actions.log", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	if _, err := os.Stat(filepath.Join(f.worktree.path, "actions.log")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat the actions' log = %v, want a retained worktree to run nothing", err)
	}
	if reports := reporter.snapshot(); len(reports) != 0 {
		t.Fatalf("reports = %+v, want none for a retained worktree", reports)
	}
}

// TestSessionReachesReadyWhenAnActionFails is the judgement a failed setup
// command gets: it is information, recorded with its code and its output, and
// not a reason to deny the reader the worktree they asked for.
func TestSessionReachesReadyWhenAnActionFails(t *testing.T) {
	t.Parallel()
	reporter := &recordingReporter{}
	f := startSessionWithActions(t, creationCheckout(workspacesession.WorktreeFresh), []workspacesession.Action{
		{ID: "act_install", Name: "Install", Command: "printf boom >&2; exit 7", RunOnWorktreeCreation: true},
	}, func(config *workspacerunner.Config) { config.Reporter = reporter })

	reports := reporter.snapshot()
	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want a running and a terminal one", reports)
	}
	terminal := reports[1]
	if terminal.Status != workspacesession.RunFailed {
		t.Fatalf("status = %q, want failed", terminal.Status)
	}
	if terminal.ExitCode == nil || *terminal.ExitCode != 7 {
		t.Fatalf("exit code = %v, want the command's own 7", terminal.ExitCode)
	}
	if !strings.Contains(terminal.Output, "boom") {
		t.Fatalf("output = %q, want what the command said", terminal.Output)
	}
	// The workspace still reached ready and still serves what it was opened
	// for: the checkout succeeded and the files are there.
	f.hub.mu.Lock()
	states := make([]string, 0, len(f.hub.heartbeats))
	for _, beat := range f.hub.heartbeats {
		states = append(states, beat.State)
	}
	f.hub.mu.Unlock()
	if len(states) == 0 || states[0] != workspacesession.StateReady {
		t.Fatalf("heartbeat states = %v, want the workspace to have reached ready", states)
	}
	f.send(t, filesFrame(t, workspacesession.TypeFilesList, "conn:1", workspacesession.FilesRequest{}))
	if answer := f.receive(t); answer.Type != workspacesession.TypeFilesListed {
		t.Fatalf("listing answered %+v, want the files channel still served", answer)
	}
}
