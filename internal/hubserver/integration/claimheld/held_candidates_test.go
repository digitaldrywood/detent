package claimheld_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type countingDetails struct {
	attachment.DetailStore
	heldReads atomic.Int64
}

func (d *countingDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	data, err := d.DetailStore.ReadDetail(ctx, organization, ref)
	if bytes.Contains(data, []byte("Approve scope")) {
		d.heldReads.Add(1)
	}
	return data, err
}

const heldReport = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve scope\n```"

type heldFixture struct {
	hubfixture.Native
	details *countingDetails
	runner  hubfixture.Runner
}

func newHeldFixture(t *testing.T) heldFixture {
	t.Helper()
	details := &countingDetails{DetailStore: hubtest.NewDetails()}
	h := hubfixture.OpenWith(t, func(config *hubfixture.Config) { config.RunDetails = details })
	p := hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: "Example project " + hubapi.NewID("fixture"), States: hubfixture.States()})
	hubfixture.ApprovePolicy(t, h, p.Base+"/policy", hubfixture.Policy())
	r := hubfixture.PrepareRunner(t, p, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Redemption.Capacity = 64
	r.Enroll(t)
	f := heldFixture{Native: p, details: details, runner: r}
	f.capacity(t)
	return f
}

func (f heldFixture) capacity(t *testing.T) {
	t.Helper()
	hubfixture.ReportRunnerCapacity(t, f.Hub, f.Base, f.runner.Redemption.Credential, f.runner.Binding.MachineID, f.runner.Redemption.DisplayName, f.runner.Redemption.BackendIsolation, f.runner.Redemption.Capacity)
}

func (f heldFixture) create(t *testing.T, title string, priority int) tracker.NativeIssue {
	t.Helper()
	draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: hubfixture.IssueBody(), State: "Todo", Priority: &priority}
	response := f.Request(t, http.MethodPost, f.Base+"/work-items", draft)
	if response.Code == http.StatusConflict {
		var challenge struct {
			Token   string `json:"challenge_token"`
			Matches []struct {
				Number int `json:"number"`
			} `json:"possible_duplicates"`
		}
		hubfixture.Decode(t, response, &challenge)
		draft.ChallengeToken = challenge.Token
		for _, match := range challenge.Matches {
			draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
		}
		response = f.Request(t, http.MethodPost, f.Base+"/work-items", draft)
	}
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func (f heldFixture) claim(t *testing.T, item tracker.NativeWorkItemID, session string) *httptest.ResponseRecorder {
	t.Helper()
	return f.Hub.Request(t, http.MethodPost, f.Base+"/claims", f.runner.Redemption.Credential, tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item, MachineID: f.runner.Binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: []string{"todo"}})
}

func (f heldFixture) hold(t *testing.T, issue tracker.NativeIssue) {
	t.Helper()
	response := f.claim(t, issue.WorkItemID, "hold-"+string(issue.WorkItemID))
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/events"
	event := hubfixture.StartedEvent(lease)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, f.runner.Redemption.Credential, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish", 2, "succeeded"
	event.Data.CompletionBody = heldReport
	event.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, f.runner.Redemption.Credential, event), http.StatusOK)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
}

func (f heldFixture) scan(t *testing.T, session string) (tracker.NativeLease, int64) {
	t.Helper()
	before := f.details.heldReads.Load()
	response := f.claim(t, "", session)
	reads := f.details.heldReads.Load() - before
	if response.Code != http.StatusOK {
		return tracker.NativeLease{}, reads
	}
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	return lease, reads
}

func TestScanClaimSkipsCurrentlyHeldItems(t *testing.T) {
	t.Parallel()
	for _, held := range []int{3, 110} {
		t.Run(fmt.Sprintf("held %d", held), func(t *testing.T) {
			f := newHeldFixture(t)
			var holds []tracker.NativeIssue
			for index := range held {
				issue := f.create(t, fmt.Sprintf("Held %d", index), 0)
				f.hold(t, issue)
				holds = append(holds, issue)
			}
			tail := f.create(t, "Eligible tail", 3)
			f.capacity(t)
			lease, readCount := f.scan(t, "scan-tail")
			if lease.WorkItemID != tail.WorkItemID || lease.FencingToken <= 0 {
				t.Fatalf("first scan claim took %+v, want the eligible tail %s", lease, tail.WorkItemID)
			}
			if readCount != 0 {
				t.Fatalf("scan claim hydrated %d held completion payloads", readCount)
			}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(lease.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			if again, _ := f.scan(t, "scan-held"); again.ID != "" && again.WorkItemID != tail.WorkItemID {
				t.Fatalf("a currently held item was claimed: %+v", again)
			}
			edited := holds[0]
			title := "Answered held item"
			hubfixture.RequireStatus(t, f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(edited.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "answer"}, ExpectedRevision: edited.Revision, Title: &title}), http.StatusOK)
			f.capacity(t)
			if reactivated, _ := f.scan(t, "scan-reactivated"); reactivated.WorkItemID != edited.WorkItemID {
				t.Fatalf("changed held item was not discoverable again: %+v", reactivated)
			}
		})
	}
}
