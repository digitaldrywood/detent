package runnerruntime_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProgressPublicationKeepsNewestEvidenceDuringLiveAttempt(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	issue := f.Create(t, "Example evidence window")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	r.Enroll(t)
	worker := r.Redemption.Credential
	path := f.Base + "/work-items/" + string(issue.WorkItemID)
	var attempts []string
	for i := range progresscheck.MaxAttempts + 2 {
		lease := seedProgressLease(t, r, issue, fmt.Sprintf("history-%d", i))
		start := hubfixture.StartedEvent(lease)
		start.IdempotencyKey = fmt.Sprintf("start-%d", i)
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", worker, start), http.StatusOK)
		finish := start
		finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", fmt.Sprintf("finish-%d", i), 2, "failed"
		for _, step := range []string{"agent_turn", "validator", "review"} {
			finish.Data.Steps = append(finish.Data.Steps, tracker.NativeStepReceipt{Step: step, PolicyID: lease.PolicyID, ReviewState: strings.Repeat("a", 64), Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("b", 40), WorkspaceDigest: strings.Repeat("c", 64)}, CompletedAt: f.Hub.Now().UTC(), CompletionBody: "Retained failure"})
		}
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
		hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
		attempts = append(attempts, start.Data.AttemptID)
	}
	var comments []string
	for i := range progresscheck.MaxComments + 2 {
		response := f.Hub.Request(t, http.MethodPost, path+"/comments", f.Token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("comment-%d", i)}, Body: "Human evidence"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var comment tracker.NativeComment
		hubfixture.Decode(t, response, &comment)
		comments = append(comments, comment.ID)
	}
	lease := seedProgressLease(t, r, issue, "live")
	params := url.Values{"policy": {lease.PolicyID}, "instance": {string(lease.MachineID)}}
	read := func() progresscheck.Request {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, path+"/progress?"+params.Encode(), worker, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var view struct {
			Request progresscheck.Request `json:"request"`
		}
		hubfixture.Decode(t, response, &view)
		return view.Request
	}
	view := read()
	if len(view.Attempts) != progresscheck.MaxAttempts || len(view.Receipts) != progresscheck.MaxReceipts || len(view.Comments) != progresscheck.MaxComments {
		t.Fatalf("evidence window: attempts=%d receipts=%d comments=%d", len(view.Attempts), len(view.Receipts), len(view.Comments))
	}
	for i, attempt := range view.Attempts {
		if attempt.ID != attempts[i+2] {
			t.Fatalf("attempt %d=%s want %s", i, attempt.ID, attempts[i+2])
		}
	}
	for i, receipt := range view.Receipts {
		if want := fmt.Sprintf("%s:step:%d", attempts[len(attempts)-8+i/3], i%3); receipt.ID != want {
			t.Fatalf("receipt %d=%s want %s", i, receipt.ID, want)
		}
	}
	for i, comment := range view.Comments {
		if comment.ID != comments[i+2] {
			t.Fatalf("comment %d=%s want %s", i, comment.ID, comments[i+2])
		}
	}
	id, err := view.ID()
	if err != nil {
		t.Fatal(err)
	}
	start := hubfixture.StartedEvent(lease)
	start.IdempotencyKey = "live-start"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/events", worker, start), http.StatusOK)
	input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, RequestID: id, PolicyID: lease.PolicyID, Instance: string(lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute, Step: "implement", Diagnosis: attempts[len(attempts)-1] + " retains a failed validation", Instruction: "Repair and verify the failing validation", Reason: "Validation requires repair", Evidence: []string{attempts[len(attempts)-1]}}}
	oversized := input
	oversized.Decision.Evidence = make([]string, progresscheck.MaxReceipts+1)
	for i := range oversized.Decision.Evidence {
		oversized.Decision.Evidence[i] = attempts[len(attempts)-1]
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/progress", worker, oversized), http.StatusUnprocessableEntity)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path+"/progress", worker, input), http.StatusOK)
	if freshID, err := read().ID(); err != nil || freshID != id {
		t.Fatalf("live attempt changed evidence: id=%s want=%s error=%v", freshID, id, err)
	}
}

func TestProgressPublicationWithoutTerminalEvidenceIsClientError(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	issue := f.Create(t, "Example without attempts")
	r := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate)
	r.Enroll(t)
	lease := seedProgressLease(t, r, issue, "no-evidence")
	input := progresscheck.Publication{Mutation: tracker.Mutation{LeaseID: lease.ID, FencingToken: lease.FencingToken}, RequestID: "invalid", PolicyID: lease.PolicyID, Instance: string(lease.MachineID), Decision: progresscheck.Decision{Outcome: progresscheck.Reroute}}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/progress", r.Redemption.Credential, input), http.StatusUnprocessableEntity)
}

func seedProgressLease(t *testing.T, r hubfixture.Runner, issue tracker.NativeIssue, session string) tracker.NativeLease {
	t.Helper()
	var row corestore.WorkItem
	if err := r.Hub.Repository.ReadTenant(t.Context(), r.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		row, err = reader.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hubtest.Lease(t, r.Hub.Repository, hubtest.SeededRunner{Identity: r.Identity}, hubtest.SeededIssue{Row: row}, hubtest.LeaseOptions{
		SessionID: session, PolicyID: hubfixture.Policy().ID, Now: r.Hub.Now(),
	})
}
