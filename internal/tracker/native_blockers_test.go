package tracker

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRecordedNativeBlockersDistinguishesDiagnosticsFromIntent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		diagnostic bool
		humanEdit  bool
		humanHold  bool
		want       bool
	}{
		{name: "completed dependency report", want: true},
		{name: "later claim refusal keeps report", diagnostic: true, want: true},
		{name: "human edit before latest runner lane invalidates report", diagnostic: true, humanEdit: true},
		{name: "same lane human hold invalidates report", diagnostic: true, humanHold: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			issue := NativeIssue{NativeReference: NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Profile: "native", Revision: 2}, State: "Blocked"}
			attempt := NativeAttempt{NativeRunData: NativeRunData{AttemptID: "attempt_example", FencingToken: 1, Identity: &NativeExecutionIdentity{Role: "code"}, Disposition: &NativeDisposition{Status: workpad.StatusBlocked}}, Status: "succeeded", WorkItemRevision: 1, StartedAt: started}
			event := func(kind string, sequence int64, data CollaborationData) CollaborationEvent {
				return CollaborationEvent{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, AggregateID: issue.WorkItemID, AggregateSequence: sequence, Type: kind, Actor: Actor{Kind: "runner"}, RecordedAt: started.Add(time.Duration(sequence) * time.Second), Data: data}
			}
			history := []CollaborationEvent{event("run.finished", 1, CollaborationData{Revision: 1, Run: &attempt.NativeRunData}), event("workflow.transitioned", 2, CollaborationData{Revision: 2, FromState: "Rework", ToState: "Blocked", Reason: "worker_progress"})}
			if test.diagnostic {
				history = append(history, event("scheduler.decision", 3, CollaborationData{Revision: 2, Decision: &NativeSchedulerDecision{Source: "native_claim_eligibility", Outcome: "skipped", Reason: "no_claimable_work", WorkItemRevision: 2}}))
			}
			if test.humanEdit {
				issue.Revision = 4
				edited := event("issue.updated", 4, CollaborationData{Revision: 3})
				edited.Actor.Kind = "user"
				history = append(history, edited, event("workflow.transitioned", 5, CollaborationData{Revision: 4, FromState: "Rework", ToState: "Blocked", Reason: "worker_progress"}))
			}
			if test.humanHold {
				held := event("workflow.transitioned", 4, CollaborationData{Revision: issue.Revision, FromState: "Blocked", ToState: "Blocked", Reason: "user_requested"})
				held.Actor.Kind = "user"
				history = append(history, held)
			}
			retained, prior, valid := RecordedNativeBlockers(issue, []NativeAttempt{attempt}, history)
			if valid != test.want {
				t.Fatalf("valid=%t want=%t", valid, test.want)
			}
			if test.want && (retained.AttemptID != attempt.AttemptID || prior != "Rework") {
				t.Fatalf("retained=%s prior=%s", retained.AttemptID, prior)
			}
		})
	}
}
