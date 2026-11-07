package hubserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProgressPublicationKeepsNewestEvidenceDuringLiveAttempt(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{RunDetails: &verifiedAttemptDetails{}})
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			f := newNativeFixture(t, f.service, f.project.OrganizationID, name)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "Example evidence window")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
			r.enroll(t)
			worker := r.redemption.Credential
			path := f.base + "/work-items/" + string(issue.WorkItemID)
			var attempts []string
			for i := range progresscheck.MaxAttempts + 2 {
				lease := nativeAttemptFixtureLease(t, r, issue, fmt.Sprintf("%s-history-%d", name, i))
				start := nativeStartedEvent(lease)
				start.IdempotencyKey = fmt.Sprintf("start-%d", i)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
				finish := start
				finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", fmt.Sprintf("finish-%d", i), 2, "failed"
				for _, step := range []string{"agent_turn", "validator", "review"} {
					finish.Data.Steps = append(finish.Data.Steps, tracker.NativeStepReceipt{Step: step, PolicyID: lease.PolicyID, ReviewState: strings.Repeat("a", 64), Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("b", 40), WorkspaceDigest: strings.Repeat("c", 64)}, CompletedAt: f.service.config.now().UTC(), CompletionBody: "Retained failure"})
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				attempts = append(attempts, start.Data.AttemptID)
			}
			var comments []string
			for i := range progresscheck.MaxComments + 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", i)}, Body: "Human evidence"})
				requireNativeStatus(t, response, http.StatusOK)
				var comment tracker.NativeComment
				decodeHubResponse(t, response, &comment)
				comments = append(comments, comment.ID)
			}
			lease := nativeAttemptFixtureLease(t, r, issue, name+"-live")
			params := url.Values{"policy": {lease.PolicyID}, "instance": {string(lease.MachineID)}}
			read := func() nativeProgressView {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, path+"/progress?"+params.Encode(), worker, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var view nativeProgressView
				decodeHubResponse(t, response, &view)
				return view
			}
			view := read()
			if len(view.Request.Attempts) != progresscheck.MaxAttempts || len(view.Request.Receipts) != progresscheck.MaxReceipts || len(view.Request.Comments) != progresscheck.MaxComments {
				t.Fatalf("evidence window: attempts=%d receipts=%d comments=%d", len(view.Request.Attempts), len(view.Request.Receipts), len(view.Request.Comments))
			}
			for i, attempt := range view.Request.Attempts {
				if attempt.ID != attempts[i+2] {
					t.Fatalf("attempt %d=%s want %s", i, attempt.ID, attempts[i+2])
				}
			}
			for i, receipt := range view.Request.Receipts {
				if want := fmt.Sprintf("%s:step:%d", attempts[len(attempts)-8+i/3], i%3); receipt.ID != want {
					t.Fatalf("receipt %d=%s want %s", i, receipt.ID, want)
				}
			}
			for i, comment := range view.Request.Comments {
				if comment.ID != comments[i+2] {
					t.Fatalf("comment %d=%s want %s", i, comment.ID, comments[i+2])
				}
			}
			id, err := view.Request.ID()
			if err != nil {
				t.Fatal(err)
			}
			start := nativeStartedEvent(lease)
			start.IdempotencyKey = "live-start"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
			input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, RequestID: id, PolicyID: lease.PolicyID, Instance: string(lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: attempts[len(attempts)-1] + " retains a failed validation", Instruction: "Repair and verify the failing validation", Reason: "Validation requires repair", Evidence: []string{attempts[len(attempts)-1]}}}
			oversized := input
			oversized.Decision.Evidence = make([]string, progresscheck.MaxReceipts+1)
			for i := range oversized.Decision.Evidence {
				oversized.Decision.Evidence[i] = attempts[len(attempts)-1]
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker, oversized), http.StatusUnprocessableEntity)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/progress", worker, input), http.StatusOK)
			if freshID, err := read().Request.ID(); err != nil || freshID != id {
				t.Fatalf("live attempt changed evidence: id=%s want=%s error=%v", freshID, id, err)
			}
		})
	}
}

func TestProgressPublicationWithoutTerminalEvidenceIsClientError(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	issue := f.create(t, "Example without attempts")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
	r.enroll(t)
	lease := nativeAttemptFixtureLease(t, r, issue, "no-evidence")
	input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, RequestID: "invalid", PolicyID: lease.PolicyID, Instance: string(lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/progress", r.redemption.Credential, input), http.StatusUnprocessableEntity)
}
