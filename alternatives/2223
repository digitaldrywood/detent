package changestaleness_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type reviewed struct {
	hubfixture.Change
	version tracker.ChangeVersion
	runner  hubfixture.Runner
}

func newReviewed(t *testing.T) reviewed {
	t.Helper()
	h := quietHub(t, nil)
	p := hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: "Example project " + hubapi.NewID("fixture"), States: hubserver.HostedProjectStates()})
	hubfixture.ApprovePolicy(t, h, p.Base+"/policy", hubfixture.Policy())
	response := p.Request(t, http.MethodPost, p.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reviewed"}, Title: "Reviewed change", Body: hubfixture.IssueBody(), State: "Human Review"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	path := p.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = p.Request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	f := reviewed{Change: hubfixture.Change{Native: p, Issue: issue, Change: change, Path: path + "/" + change.ID}}
	f.version = f.PublishSource(t, "publish", "", func(input *tracker.ChangeVersionInput) []byte { return sourcetest.Source(t, input) })
	hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, f.Path+"/versions/"+f.version.ID+"/reviews", tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	f.runner = hubfixture.PrepareRunner(t, p, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	f.runner.Enroll(t)
	f.runner.ReportCapacity(t)
	h.RefreshBoard(t, p.Organization())
	if status := f.storedStatus(t); status != "reviewed" {
		t.Fatalf("fixture card status=%q, want reviewed", status)
	}
	return f
}

func (f reviewed) storedStatus(t *testing.T) string {
	t.Helper()
	var row viewstore.BoardRecord
	if err := f.Hub.Repository.ReadTenant(t.Context(), f.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.BoardItem(t.Context(), f.Project.ID, f.Issue.WorkItemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if row.Display.Change == nil {
		return ""
	}
	return row.Display.Change.Status
}

func (f reviewed) approve(t *testing.T, descriptor policy.Descriptor) {
	t.Helper()
	hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPut, f.Base+"/policy", f.Hub.AdminToken(), policy.Change{ExpectedID: hubfixture.Policy().ID, Policy: descriptor}), http.StatusOK)
}

func (f reviewed) claim(t *testing.T) int {
	t.Helper()
	f.runner.ReportCapacity(t)
	response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", f.runner.Redemption.Credential, tracker.NativeClaim{PolicyID: f.currentPolicy(t), WorkItemID: f.Issue.WorkItemID, MachineID: f.runner.Binding.MachineID, SessionID: "landing-" + hubapi.NewID("session"), TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
	return response.Code
}

func (f reviewed) currentPolicy(t *testing.T) string {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, f.Base+"/policy", f.Hub.AdminToken(), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var approval policy.Approval
	hubfixture.Decode(t, response, &approval)
	return approval.Policy.ID
}

func TestStaleVersionRefusedWhileCardSaysReviewed(t *testing.T) {
	t.Parallel()
	t.Run("requirement change", func(t *testing.T) {
		f := newReviewed(t)
		changed := hubfixture.Policy()
		changed.Gates.SecurityAudit = true
		changed = changed.WithID()
		f.approve(t, changed)
		if status := f.storedStatus(t); status != "reviewed" {
			t.Fatalf("approval rebuilt the card: status=%q", status)
		}
		if code := f.claim(t); code == http.StatusOK {
			t.Fatal("a landing claim was granted for a version the new policy made stale")
		}
		response := f.Request(t, http.MethodPost, f.Path+"/versions/"+f.version.ID+"/reviews", tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-again"}, Decision: "approved"})
		if response.Code == http.StatusOK {
			var detail tracker.ChangeDetail
			hubfixture.Decode(t, response, &detail)
			if detail.Summary.Status == "reviewed" {
				t.Fatalf("review owner reported a stale version as reviewed: %+v", detail.Summary)
			}
		}
		response = f.Request(t, http.MethodGet, f.Path, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var detail tracker.ChangeDetail
		hubfixture.Decode(t, response, &detail)
		if detail.Summary.Status != "stale_policy" {
			t.Fatalf("change detail status=%q, want stale_policy", detail.Summary.Status)
		}
	})
	t.Run("instruction-only change", func(t *testing.T) {
		f := newReviewed(t)
		instructions := hubfixture.Policy()
		instructions.SourceDigest = policy.Digest([]byte("changed instructions"))
		instructions = instructions.WithID()
		f.approve(t, instructions)
		if code := f.claim(t); code != http.StatusOK {
			t.Fatalf("instruction-only policy change refused the landing claim: status=%d", code)
		}
	})
}
