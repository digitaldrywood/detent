package hubserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// Handing a queued run to the runner (decisions section 18.12).
//
// The eighth dogfood run queued a run through the API, got its 202, and watched
// the row sit queued for three minutes and forty seconds: the process only ever
// started when a browser opened the exec channel for it, and the runner's own
// lane runs a project's actions at worktree creation and never again. These
// tests are the rule that closes that hole -- a queued run on a workspace the
// runner holds is executed by the runner, whether or not anybody is watching --
// and the claim that keeps one row from becoming two processes.

// dispatch runs one tick of the hub's own dispatch.
func (f *relayFixture) dispatch(t *testing.T) {
	t.Helper()
	f.service.workspaces.dispatchQueuedActionRuns(t.Context())
}

// waitedOutTheGrace moves the fixture clock past the window a run is left for
// the person who asked for it.
func (f *relayFixture) waitedOutTheGrace() { f.advance(actionRunDispatchGrace + time.Second) }

// workspaceStreamCount reports how many streams the workspace's room holds,
// attached and detached. It reads the relay directly because the assertion is
// about a budget nothing on the wire reports.
func (f *relayFixture) workspaceStreamCount() int {
	relay := f.service.workspaces.relay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	room := relay.rooms[f.workspace]
	if room == nil {
		return 0
	}
	return len(room.streams) + len(room.detached)
}

// decodeFramePayload reads a frame's payload into target.
func decodeFramePayload(t *testing.T, frame workspacesession.Frame, target any) {
	t.Helper()
	if err := json.Unmarshal(frame.Payload, target); err != nil {
		t.Fatalf("decode %s payload: %v", frame.Type, err)
	}
}

func TestWorkspaceHubDispatchesAQueuedRunToTheRunner(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	action := f.action(t, "Greet", "echo hello")
	// No person connection at all: this is the headless caller's run, the one
	// that never executed before.
	run := f.queuedRun(t, action)
	f.waitedOutTheGrace()

	f.dispatch(t)
	handed := runner.receive()
	if handed.Channel != workspacesession.ChannelExec || handed.Type != workspacesession.TypeExecRun {
		t.Fatalf("the runner received %+v, want a run frame on the exec channel", handed)
	}
	if handed.Seq != 1 || handed.Stream == "" {
		t.Fatalf("the run frame = %+v, want its own stream and sequence", handed)
	}
	// No actor stamp: section 18.2's stamp says which person asked, and nobody
	// did. Inventing one would name the wrong thing in the runner's audit.
	if handed.Actor != nil {
		t.Fatalf("the run frame carried actor %+v, want none", handed.Actor)
	}
	var carried workspacesession.ExecRun
	decodeFramePayload(t, handed, &carried)
	if carried.RunID != run || carried.ActionID != action.ID || carried.Command != action.Command {
		t.Fatalf("the run frame carried %+v, want the queued run and the project's own command", carried)
	}

	// The row is claimed before the frame goes out, so a runner holding the
	// frame is serving a run the hub has already marked running.
	started := f.runRow(t, run)
	if started.Status != workspacesession.RunRunning || started.StartedAt == nil {
		t.Fatalf("run = %#v, want running with a start time", started)
	}
	if started.ClaimedBy != handed.Stream || !strings.HasPrefix(started.ClaimedBy, hubStreamOwner+":") {
		t.Fatalf("claimed_by = %q, want the hub's own stream %q", started.ClaimedBy, handed.Stream)
	}

	// And the hub records the run from the frames it is relaying, exactly as it
	// records a person's, even though there is no person to relay them to.
	runner.send(execOutputFrame(handed.Stream, workspacesession.ExecOutput{Data: "hello\n"}))
	runner.send(execExitedFrame(handed.Stream, 0))
	waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunSucceeded })
	finished := f.runRow(t, run)
	if finished.ExitCode == nil || *finished.ExitCode != 0 || finished.FinishedAt == nil {
		t.Fatalf("run = %#v, want exit 0 and a finish time", finished)
	}
	if finished.Output != "hello\n" || finished.OutputBytes != int64(len("hello\n")) {
		t.Fatalf("run = %#v, want the output recorded", finished)
	}
	// The whole point of recording it: a browser that opens the Output surface
	// afterwards reads this run back through its own endpoint.
	requireLastRunEvent(t, f.runEventTypes(t, run), "action_run.succeeded")
}

func TestWorkspaceHubDispatchLeavesARunAlone(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// prepare arranges the fixture and answers the run that must stay
		// queued.
		prepare func(t *testing.T, f *relayFixture, action workspacesession.Action) string
	}{
		{
			name: "inside the grace period",
			prepare: func(t *testing.T, f *relayFixture, action workspacesession.Action) string {
				// The person who asked gets the window, because only their own
				// stream can show them the output as it is produced.
				t.Helper()
				return f.queuedRun(t, action)
			},
		},
		{
			name: "on a workspace held read-only",
			prepare: func(t *testing.T, f *relayFixture, action workspacesession.Action) string {
				t.Helper()
				run := f.queuedRun(t, action)
				// A workspace can become the one an attempt is editing after a
				// run was queued on it, and a command must not write into a
				// worktree a model is using.
				if _, err := f.service.database.db.ExecContext(t.Context(),
					"UPDATE workspace_sessions SET read_only = 1 WHERE id = ?", f.workspace); err != nil {
					t.Fatal(err)
				}
				f.waitedOutTheGrace()
				return run
			},
		},
		{
			name: "on a workspace no longer bound",
			prepare: func(t *testing.T, f *relayFixture, action workspacesession.Action) string {
				t.Helper()
				run := f.queuedRun(t, action)
				if _, err := f.service.database.db.ExecContext(t.Context(),
					"UPDATE workspace_sessions SET state = 'closing' WHERE id = ?", f.workspace); err != nil {
					t.Fatal(err)
				}
				f.waitedOutTheGrace()
				return run
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newRelayFixture(t, withExec)
			// The runner is attached in every case, so each of these refusals
			// is the rule under test rather than "there was nobody to send it
			// to" wearing the same clothes.
			f.dialRunner(t)
			action := f.action(t, "Greet", "echo hello")
			run := test.prepare(t, f, action)

			f.dispatch(t)
			// A frame that was never sent cannot be waited for, so the
			// assertion is on the row: it is still queued, still unclaimed, and
			// no stream was spent on it.
			left := f.runRow(t, run)
			if left.Status != workspacesession.RunQueued || left.ClaimedBy != "" || left.Revision != 1 {
				t.Fatalf("run = %#v, want it left queued and unclaimed", left)
			}
			// And no stream was spent on it either, so a workspace whose runs
			// are all refused does not slowly exhaust its stream budget.
			if streams := f.workspaceStreamCount(); streams != 0 {
				t.Fatalf("the workspace holds %d streams, want none opened", streams)
			}
		})
	}
}

func TestWorkspaceHubDispatchAndAPersonNeverBothStartOneRun(t *testing.T) {
	t.Parallel()
	f := newRelayFixture(t, withExec)
	runner := f.dialRunner(t)
	person := f.dialPerson(t)
	action := f.action(t, "Greet", "echo hello")

	t.Run("the hub refuses a person the run it already dispatched", func(t *testing.T) {
		run := f.queuedRun(t, action)
		f.waitedOutTheGrace()
		f.dispatch(t)
		handed := runner.receive()

		// The person arrives late with the frame they would have sent. The row
		// is already running on the hub's own stream, so they are told which
		// refusal this is: the run is going to run, and the answer is to read
		// it back rather than to ask again.
		person.send(execRunFrame(run, action))
		if payload := errorPayload(t, person.receive()); payload.Code != workspacesession.CodeAlreadyRunning {
			t.Fatalf("the late person answered %+v, want already_running", payload)
		}
		if claimed := f.runRow(t, run); claimed.ClaimedBy != handed.Stream {
			t.Fatalf("claimed_by = %q, want the hub's stream %q -- the first claim wins", claimed.ClaimedBy, handed.Stream)
		}
		// One executor means one process: the runner was handed the run once.
		runner.send(execExitedFrame(handed.Stream, 0))
		waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunSucceeded })
	})

	t.Run("the hub does not dispatch a run a person already started", func(t *testing.T) {
		run := f.queuedRun(t, action)
		person.send(execRunFrame(run, action))
		handed := runner.receive()
		if !strings.HasPrefix(handed.Stream, "relayconn_") {
			t.Fatalf("the run went out on %q, want the person's own stream", handed.Stream)
		}
		f.waitedOutTheGrace()

		f.dispatch(t)
		// The dispatch's own read excludes a claimed row, so nothing is sent
		// and nothing is rewritten: the person's stream is still the one
		// carrying this run.
		if claimed := f.runRow(t, run); claimed.ClaimedBy != handed.Stream || claimed.Revision != 2 {
			t.Fatalf("run = %#v, want it still claimed by the person's stream at revision 2", claimed)
		}
		runner.send(execExitedFrame(handed.Stream, 0))
		waitFor(t, func() bool { return f.runRow(t, run).Status == workspacesession.RunSucceeded })
	})
}
