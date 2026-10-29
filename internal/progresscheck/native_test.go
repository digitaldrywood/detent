package progresscheck

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRequestBoundsAndAuditBindings(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		text string
	}{
		{"large text", strings.Repeat("evidence ", MaxInputBytes)},
		{"JSON escaped text", strings.Repeat("\x00", MaxInputBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			recovery := tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "example-project", WorkItemID: "example-issue", Revision: 2}, Body: test.text}}
			for i := range 30 {
				recovery.Attempts = append(recovery.Attempts, tracker.NativeAttempt{NativeRunData: tracker.NativeRunData{AttemptID: fmt.Sprintf("attempt-%d", i), Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: test.text, Stage: "publication", Attribution: "instance", ErrorClass: "publication"}}, Checkpoint: &tracker.NativeCheckpoint{HeadSHA: "head", BaseSHA: "base", CommitSHA: "checkpoint"}})
				recovery.Discussion = append(recovery.Discussion, tracker.NativeComment{ID: fmt.Sprintf("comment-%d", i), Revision: 1, Body: test.text, Actor: tracker.Actor{Kind: "human"}})
			}
			recovery.ChangeDetail = &tracker.ChangeDetail{Reviews: []tracker.ChangeReview{{ID: "audit", SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", Run: securityaudit.Run{HeadSHA: "audited-head", BaseSHA: "audited-base"}}}}}
			request := NativeRequest(recovery, "policy", "instance")
			if len(request.Attempts) != MaxAttempts || request.Attempts[0].ID != "attempt-18" || len(request.Comments) != MaxComments {
				t.Fatalf("request bounds: attempts=%d comments=%d", len(request.Attempts), len(request.Comments))
			}
			if request.Attempts[0].Attribution != "instance" || request.Attempts[0].CheckpointSHA != "checkpoint" {
				t.Fatal("attribution or checkpoint lost")
			}
			if len(request.Receipts) != 1 || request.Receipts[0].Step != "security_audit" || request.Receipts[0].BaseSHA != "audited-base" || request.Receipts[0].HeadSHA != "audited-head" || request.Receipts[0].Outcome != "pass" {
				t.Fatalf("audit bindings=%+v", request.Receipts)
			}
			encoded, err := json.Marshal(request)
			if err != nil || len(encoded) > MaxInputBytes {
				t.Fatalf("input bytes=%d err=%v", len(encoded), err)
			}
			if _, err := request.ID(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeRequestActiveAttemptDoesNotRetryOlderFailure(t *testing.T) {
	t.Parallel()
	recovery := tracker.NativeRecovery{Attempts: []tracker.NativeAttempt{
		{NativeRunData: tracker.NativeRunData{AttemptID: "failed", Outcome: "failed"}, Status: "failed"},
		{NativeRunData: tracker.NativeRunData{AttemptID: "current"}, Status: "running"},
	}}
	backend := &fakeBackend{}
	_, checked, err := Check(t.Context(), NativeRequest(recovery, "policy", "instance"), backend, &fakeOwner{})
	if err != nil || checked || backend.calls != 0 {
		t.Fatalf("active continuation paid an AI turn: checked=%v calls=%d error=%v", checked, backend.calls, err)
	}
}

func TestNativeEmptyLandingSuccessRequiresProgressCheck(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"succeeded", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			recovery := tracker.NativeRecovery{
				Issue:    tracker.NativeIssue{NativeReference: tracker.NativeReference{ProjectID: "example-project", WorkItemID: "example-issue", Revision: 1}},
				Attempts: []tracker.NativeAttempt{{NativeRunData: tracker.NativeRunData{AttemptID: "merge-attempt", Outcome: outcome, Identity: &tracker.NativeExecutionIdentity{Role: "merge"}, Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", Refusal: "the published source has no deliverable to verify", BaseSHA: "base", HeadSHA: "head"}}}}},
			}
			backend := &fakeBackend{decision: Decision{Outcome: Reroute, Step: "implement", Reason: "Acceptance remains unmet after nothing_to_land", Evidence: []string{"merge-attempt:landing"}}}
			_, checked, err := Check(t.Context(), NativeRequest(recovery, "policy", "instance"), backend, &fakeOwner{})
			if err != nil || !checked || backend.calls != 1 {
				t.Fatalf("empty landing skipped progress check: checked=%v calls=%d error=%v", checked, backend.calls, err)
			}
		})
	}
}

func TestNativeCompletionAvailabilityUsesCurrentReviewedEmptySource(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		current string
		status  string
		empty   bool
		want    bool
	}{
		{name: "reviewed empty source", current: "empty", status: "reviewed", empty: true, want: true},
		{name: "review pending", current: "empty", status: "review_required", empty: true},
		{name: "audit rejected", current: "empty", status: "changes_requested", empty: true},
		{name: "older empty source", current: "current", status: "reviewed", empty: true},
		{name: "reviewed deliverable", current: "empty", status: "reviewed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := tracker.ChangeSourceDigest([]byte("delta"))
			if test.empty {
				digest = tracker.ChangeSourceDigest(nil)
			}
			recovery := tracker.NativeRecovery{ChangeDetail: &tracker.ChangeDetail{Change: tracker.ChangeRequest{CurrentVersion: test.current}, Summary: tracker.ChangeSummary{Status: test.status}, Versions: []tracker.ChangeVersion{{ID: "empty", ChangeVersionInput: tracker.ChangeVersionInput{Source: &tracker.ChangeSource{DiffSHA256: digest}}}}}}
			request := NativeRequest(recovery, "policy", "instance")
			available := false
			for _, step := range request.AvailableSteps {
				available = available || step == CompleteStep
			}
			if available != test.want {
				t.Fatalf("completion available=%v want=%v request=%+v", available, test.want, request)
			}
		})
	}
}
