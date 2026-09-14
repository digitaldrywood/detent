package hubserver

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// A provider thread carries the instructions and the permission set of the
// turn that opened it. A coordinator turn runs read-only with no checkout and
// no authority to change files, run tests or move issue state; a worker
// attempt runs with all of them. The September 12 dogfood run caught what
// happens when the boundary is not enforced: an issue attempt resumed the
// coordinator's thread on the same runner and answered "This session's
// coordinator restrictions prohibit editing files, running tests, or changing
// issue state, so I can't execute the worker assignment" while still
// reporting a succeeded outcome.
//
// The rule is origin equality: a thread is resumed only by the kind of turn
// that opened it (decisions section 9.3).
func TestConversationResumeDecision(t *testing.T) {
	t.Parallel()
	const (
		thisRunner  = "rnr_this"
		otherRunner = "rnr_other"
	)
	for _, test := range []struct {
		name     string
		decision conversationResumeDecision
		want     bool
	}{
		{
			name: "a worker attempt is handed a transcript rather than the coordinator's thread",
			decision: conversationResumeDecision{
				RecordedRunnerID: thisRunner, RecordedOrigin: conversation.ThreadOriginCoordinator,
				BindingRunnerID: thisRunner, Coordinator: false,
			},
			want: false,
		},
		{
			name: "a worker attempt resumes the worker thread it produced",
			decision: conversationResumeDecision{
				RecordedRunnerID: thisRunner, RecordedOrigin: conversation.ThreadOriginWorker,
				BindingRunnerID: thisRunner, Coordinator: false,
			},
			want: true,
		},
		{
			name: "a coordinator turn continues its own coordinator thread",
			decision: conversationResumeDecision{
				RecordedRunnerID: thisRunner, RecordedOrigin: conversation.ThreadOriginCoordinator,
				BindingRunnerID: thisRunner, Coordinator: true,
			},
			want: true,
		},
		{
			name: "a coordinator turn does not inherit a worker thread's authority",
			decision: conversationResumeDecision{
				RecordedRunnerID: thisRunner, RecordedOrigin: conversation.ThreadOriginWorker,
				BindingRunnerID: thisRunner, Coordinator: true,
			},
			want: false,
		},
		{
			name: "another runner's thread is on another provider login and never resumes",
			decision: conversationResumeDecision{
				RecordedRunnerID: otherRunner, RecordedOrigin: conversation.ThreadOriginWorker,
				BindingRunnerID: thisRunner, Coordinator: false,
			},
			want: false,
		},
		{
			name: "a thread recorded before the origin was known never resumes",
			decision: conversationResumeDecision{
				RecordedRunnerID: thisRunner, RecordedOrigin: "",
				BindingRunnerID: thisRunner, Coordinator: false,
			},
			want: false,
		},
		{
			name: "a credential with no runner identity inherits the hub-side coordinator's thread from nobody",
			decision: conversationResumeDecision{
				RecordedRunnerID: "", RecordedOrigin: conversation.ThreadOriginCoordinator,
				BindingRunnerID: "", Coordinator: true,
			},
			want: false,
		},
		{
			name: "a runner that names the thread on this bind is holding its own",
			decision: conversationResumeDecision{
				RunnerThreadID: "thread-named-by-the-runner", RecordedRunnerID: otherRunner,
				RecordedOrigin: conversation.ThreadOriginCoordinator, BindingRunnerID: thisRunner, Coordinator: false,
			},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.decision.resumeThread(); got != test.want {
				t.Fatalf("resumeThread() = %v, want %v for %#v", got, test.want, test.decision)
			}
		})
	}
}

// ThreadOriginFor is the one place a turn's kind becomes the origin it
// records, so the two callers that write the column cannot drift apart.
func TestThreadOriginFor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		coordinator bool
		want        string
	}{
		{name: "a coordinator turn opens a coordinator thread", coordinator: true, want: "coordinator"},
		{name: "a worker attempt opens a worker thread", coordinator: false, want: "worker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := conversation.ThreadOriginFor(test.coordinator); got != test.want {
				t.Fatalf("ThreadOriginFor(%v) = %q, want %q", test.coordinator, got, test.want)
			}
		})
	}
}

// TestWorkerAttemptDoesNotResumeTheCoordinatorThread is the September 12
// dogfood defect end to end: one coordinator turn opens a thread on a runner,
// the chat is then linked to an issue, and the issue attempt lands on the same
// runner. Before the fix the bind answered with resume "thread" and that same
// thread id, and Codex refused the worker assignment because the thread still
// carried the coordinator's restrictions.
func TestWorkerAttemptDoesNotResumeTheCoordinatorThread(t *testing.T) {
	t.Parallel()
	f := newCoordinatorItemFixture(t)
	live := f.runner(t, true)

	// One coordinator turn, which opens the conversation's provider thread.
	coordinator := f.start(t, live, f.item, "coordinator-1")
	bound := f.bind(t, coordinator)
	if !bound.Coordinator || bound.Resume.ThreadOrigin != conversation.ThreadOriginCoordinator {
		t.Fatalf("coordinator bind resume = %#v, want a coordinator origin", bound.Resume)
	}
	requireNativeStatus(t, f.turnEvents(t, coordinator,
		map[string]any{"type": "turn_started", "turn_id": "turn-1", "thread_id": "coordinator-thread"}), http.StatusAccepted)
	if origin := f.threadOrigin(t); origin != conversation.ThreadOriginCoordinator {
		t.Fatalf("recorded thread origin = %q, want coordinator", origin)
	}
	requireNativeStatus(t, f.unbind(t, coordinator, "succeeded"), http.StatusOK)
	f.release(t, live, coordinator.lease)

	// The chat is linked to an issue and the attempt runs on the same runner.
	response := f.link(t, f.token, f.conversationID, "link-1", true, "Make the test pass")
	requireNativeStatus(t, response, http.StatusOK)
	var linked conversationLinkResponse
	decodeHubResponse(t, response, &linked)

	worker := f.start(t, live, linked.Issue.ID, "worker-1")
	workerBound := f.bind(t, worker)
	if workerBound.Coordinator {
		t.Error("the linked issue bound as a coordinator item")
	}
	if workerBound.Resume.ThreadID != "" {
		t.Fatalf("the worker attempt resumed %q, the thread a coordinator turn opened", workerBound.Resume.ThreadID)
	}
	if workerBound.Resume.ThreadOrigin != conversation.ThreadOriginWorker {
		t.Errorf("worker bind thread origin = %q, want worker", workerBound.Resume.ThreadOrigin)
	}
	if len(workerBound.Resume.Transcript) == 0 {
		t.Error("the worker attempt was given neither the thread nor the shared history")
	}

	// Its own thread is recorded with the worker origin, and a later worker
	// attempt on the same runner resumes that one.
	requireNativeStatus(t, f.turnEvents(t, worker,
		map[string]any{"type": "turn_started", "turn_id": "turn-2", "thread_id": "worker-thread"}), http.StatusAccepted)
	if origin := f.threadOrigin(t); origin != conversation.ThreadOriginWorker {
		t.Fatalf("recorded thread origin after the worker turn = %q, want worker", origin)
	}
	requireNativeStatus(t, f.unbind(t, worker, "succeeded"), http.StatusOK)
	f.release(t, live, worker.lease)

	next := f.start(t, live, linked.Issue.ID, "worker-2")
	rebound := f.bind(t, next)
	if rebound.Resume.ThreadID != "worker-thread" || len(rebound.Resume.Transcript) != 0 {
		t.Fatalf("second worker bind resume = %#v, want the worker thread it produced", rebound.Resume)
	}
	if rebound.Resume.ThreadOrigin != conversation.ThreadOriginWorker {
		t.Errorf("second worker bind thread origin = %q, want worker", rebound.Resume.ThreadOrigin)
	}
}

// threadOrigin reads the origin recorded on the conversation's provider
// thread.
func (f *coordinatorItemFixture) threadOrigin(t *testing.T) string {
	t.Helper()
	var origin string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT provider_thread_origin FROM conversations WHERE id = ?", f.conversationID).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

// release gives a claim back so the next turn on the same runner can take one.
func (f *coordinatorItemFixture) release(t *testing.T, r runnerFixture, lease tracker.NativeLease) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release",
		r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}
