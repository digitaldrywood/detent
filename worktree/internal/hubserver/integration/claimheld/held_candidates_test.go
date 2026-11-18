package claimheld_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

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
	trace   *candidateTrace
	runner  hubfixture.Runner
}

func newHeldFixture(t *testing.T) heldFixture {
	t.Helper()
	details := &countingDetails{DetailStore: hubtest.NewDetails()}
	trace := &candidateTrace{}
	h := hubfixture.OpenPool(t, hubfixture.Config{RunDetails: details}, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, trace)
	})
	return newHeldProject(t, h, "", details, trace, hubfixture.States())
}

func newHeldProject(t *testing.T, h *hubfixture.Hub, organization tracker.OrganizationID, details *countingDetails, trace *candidateTrace, states []tracker.NativeState) heldFixture {
	t.Helper()
	p := hubfixture.NewNativeWith(t, h, organization, hubtest.ProjectOptions{Name: "Example project " + hubapi.NewID("fixture"), States: states})
	hubfixture.ApprovePolicy(t, h, p.Base+"/policy", hubfixture.Policy())
	f := heldFixture{Native: p, details: details, trace: trace}
	f.runner = f.enroll(t)
	return f
}

func (f heldFixture) enroll(t *testing.T) hubfixture.Runner {
	t.Helper()
	r := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	r.Redemption.Capacity = 64
	r.Enroll(t)
	hubfixture.ReportRunnerCapacity(t, f.Hub, f.Base, r.Redemption.Credential, r.Binding.MachineID, r.Redemption.DisplayName, r.Redemption.BackendIsolation, r.Redemption.Capacity)
	return r
}

func (f heldFixture) capacity(t *testing.T, runners ...hubfixture.Runner) {
	t.Helper()
	if len(runners) == 0 {
		runners = []hubfixture.Runner{f.runner}
	}
	for _, r := range runners {
		hubfixture.ReportRunnerCapacity(t, f.Hub, f.Base, r.Redemption.Credential, r.Binding.MachineID, r.Redemption.DisplayName, r.Redemption.BackendIsolation, r.Redemption.Capacity)
	}
}

func (f heldFixture) create(t *testing.T, title string, priority int) tracker.NativeIssue {
	t.Helper()
	return f.createIn(t, title, priority, "Todo")
}

func (f heldFixture) createIn(t *testing.T, title string, priority int, state string) tracker.NativeIssue {
	t.Helper()
	draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + title}, Title: title, Body: hubfixture.IssueBody(), State: state, Priority: &priority}
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
	return f.claimAs(t, f.runner, item, session, "todo")
}

func (f heldFixture) claimAs(t *testing.T, r hubfixture.Runner, item tracker.NativeWorkItemID, session string, states ...string) *httptest.ResponseRecorder {
	t.Helper()
	return f.Hub.Request(t, http.MethodPost, f.Base+"/claims", r.Redemption.Credential, tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item, MachineID: r.Binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: states})
}

func (f heldFixture) hold(t *testing.T, issue tracker.NativeIssue) {
	t.Helper()
	response := f.claim(t, issue.WorkItemID, "hold-"+string(issue.WorkItemID))
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, response, &lease)
	path := f.Base + "/work-items/" + string(issue.WorkItemID) + "/events"
	event := hubfixture.StartedEvent(lease)
	event.IdempotencyKey = "start-" + string(issue.WorkItemID)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, f.runner.Redemption.Credential, event), http.StatusOK)
	event.Type, event.IdempotencyKey, event.Data.Sequence, event.Data.Outcome = "run.finished", "finish-"+string(issue.WorkItemID), 2, "succeeded"
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

func TestHeldItemReactivatedBySupersedingAttempt(t *testing.T) {
	t.Parallel()
	f := newHeldFixture(t)
	reactivated := f.create(t, "Superseded hold", 0)
	f.hold(t, reactivated)
	unresolved := f.create(t, "Unresolved hold", 0)
	f.hold(t, unresolved)
	title := "Superseded hold answered"
	hubfixture.RequireStatus(t, f.Request(t, http.MethodPatch, f.Base+"/work-items/"+string(reactivated.WorkItemID), tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "supersede"}, ExpectedRevision: reactivated.Revision, Title: &title}), http.StatusOK)
	f.capacity(t)
	superseding, _ := f.scan(t, "superseding")
	if superseding.WorkItemID != reactivated.WorkItemID {
		t.Fatalf("changed hold was not claimable: %+v", superseding)
	}
	path := f.Base + "/work-items/" + string(reactivated.WorkItemID) + "/events"
	started := hubfixture.StartedEvent(superseding)
	started.IdempotencyKey = "superseding-start"
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, path, f.runner.Redemption.Credential, started), http.StatusOK)
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(superseding.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: superseding.FencingToken, Reason: "released"}), http.StatusNoContent)
	f.capacity(t)
	again, _ := f.scan(t, "after-superseding")
	if again.WorkItemID != reactivated.WorkItemID || again.ID == superseding.ID {
		t.Fatalf("item with a superseding attempt stayed hidden: %+v", again)
	}
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, f.Base+"/leases/"+string(again.ID)+"/release", f.runner.Redemption.Credential, tracker.NativeLeaseMutation{FencingToken: again.FencingToken, Reason: "released"}), http.StatusNoContent)
	f.capacity(t)
	if response := f.claim(t, "", "only-unresolved"); response.Code == http.StatusOK {
		var lease tracker.NativeLease
		hubfixture.Decode(t, response, &lease)
		if lease.WorkItemID == unresolved.WorkItemID {
			t.Fatalf("unresolved hold was claimed: %+v", lease)
		}
	}
}
