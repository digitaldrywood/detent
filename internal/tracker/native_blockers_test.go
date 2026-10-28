package tracker

import (
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRecordedNativeBlockersNewestAttemptEquivalence(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*NativeIssue, *NativeAttempt, *[]CollaborationEvent)
		valid  bool
	}{
		{"current report supersedes earlier human edit", func(*NativeIssue, *NativeAttempt, *[]CollaborationEvent) {}, true},
		{"archived issue", func(issue *NativeIssue, _ *NativeAttempt, _ *[]CollaborationEvent) { issue.Archived = true }, false},
		{"different profile", func(issue *NativeIssue, _ *NativeAttempt, _ *[]CollaborationEvent) { issue.Profile = "external" }, false},
		{"different lane", func(issue *NativeIssue, _ *NativeAttempt, _ *[]CollaborationEvent) { issue.State = "Todo" }, false},
		{"newest failed", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) { attempt.Status = "failed" }, false},
		{"newest has no disposition", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) { attempt.Disposition = nil }, false},
		{"newest not blocked", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) {
			attempt.Disposition.Status = "completed"
		}, false},
		{"missing identity", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) { attempt.Identity = nil }, false},
		{"missing revision", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) { attempt.WorkItemRevision = 0 }, false},
		{"future report", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) { (*history)[0].Data.Revision = 8 }, false},
		{"human edit after report", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) { (*history)[1].Data.Revision = 7 }, false},
		{"missing finish history", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) { *history = (*history)[1:] }, false},
		{"finish from another organization", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[0].OrganizationID = "org_other"
		}, false},
		{"finish from another project", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[0].ProjectID = "prj_other"
		}, false},
		{"finish from another item", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[0].AggregateID = "wi_other"
		}, false},
		{"finish from another fence", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[0].Data.Run.FencingToken = 2
		}, false},
		{"human lane transition", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[2].Actor.Kind = "human"
		}, false},
		{"stale lane revision", func(_ *NativeIssue, _ *NativeAttempt, history *[]CollaborationEvent) { (*history)[2].Data.Revision = 6 }, false},
		{"lane predates attempt", func(_ *NativeIssue, attempt *NativeAttempt, history *[]CollaborationEvent) {
			(*history)[2].RecordedAt = attempt.StartedAt.Add(-time.Second)
		}, false},
		{"no attempts", func(_ *NativeIssue, attempt *NativeAttempt, _ *[]CollaborationEvent) { *attempt = NativeAttempt{} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
			issue := NativeIssue{NativeReference: NativeReference{OrganizationID: "org_example", ProjectID: "prj_example", WorkItemID: "wi_example", Profile: "native", Revision: 7}, State: "Blocked"}
			latest := NativeAttempt{NativeRunData: NativeRunData{AttemptID: "attempt_latest", FencingToken: 9, Identity: &NativeExecutionIdentity{Backend: "example"}, Disposition: &NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true}}, Status: "succeeded", WorkItemRevision: 3, StartedAt: now}
			history := []CollaborationEvent{
				{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, AggregateID: issue.WorkItemID, Type: "run.finished", Actor: Actor{Kind: "runner"}, Data: CollaborationData{Revision: 6, Run: &NativeRunData{AttemptID: latest.AttemptID, FencingToken: latest.FencingToken}}},
				{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, AggregateID: issue.WorkItemID, Type: "issue.edited", Actor: Actor{Kind: "human"}, Data: CollaborationData{Revision: 5}},
				{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, AggregateID: issue.WorkItemID, Type: "workflow.transitioned", AggregateSequence: 10, RecordedAt: now, Actor: Actor{Kind: "runner"}, Data: CollaborationData{Revision: 7, FromState: "In Progress", ToState: "Blocked", Reason: "worker_progress"}},
			}
			test.change(&issue, &latest, &history)
			older := NativeAttempt{NativeRunData: NativeRunData{AttemptID: "attempt_older", FencingToken: 1, Identity: &NativeExecutionIdentity{Backend: "example"}, Disposition: &NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true}}, Status: "succeeded", WorkItemRevision: 3, StartedAt: now}
			middle := older
			middle.AttemptID, middle.FencingToken = "attempt_middle", 5
			full := []NativeAttempt{middle, latest, older}
			newest := []NativeAttempt{latest}
			if test.name == "no attempts" {
				full, newest = nil, nil
			}
			want, wantState, wantValid := RecordedNativeBlockers(issue, full, history)
			got, gotState, gotValid := RecordedNativeBlockers(issue, newest, history)
			if gotValid != test.valid || wantValid != test.valid || gotState != wantState || !reflect.DeepEqual(got, want) {
				t.Fatalf("newest valid=%t state=%q, full valid=%t state=%q, want valid=%t", gotValid, gotState, wantValid, wantState, test.valid)
			}
			if gotValid && (got.AttemptID != "attempt_latest" || gotState != "In Progress") {
				t.Fatalf("selected attempt=%s state=%s", got.AttemptID, gotState)
			}
		})
	}
}
