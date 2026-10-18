package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
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
