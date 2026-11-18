package claimdecision_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimDetails struct {
	attachment.DetailStore
	mu      sync.Mutex
	onClaim func()
}

func (d *claimDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	d.mu.Lock()
	hook := d.onClaim
	if hook != nil && bytes.Contains(data, []byte(`"source":"native_claim"`)) && bytes.Contains(data, []byte(`"outcome":"claimed"`)) {
		d.onClaim = nil
	} else {
		hook = nil
	}
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return d.DetailStore.StoreDetail(ctx, organization, data)
}

type landing struct {
	hubfixture.Change
	version tracker.ChangeVersion
	runner  hubfixture.Runner
}

func newLanding(t *testing.T, h *hubfixture.Hub) landing {
	t.Helper()
	p := hubfixture.NewNativeWith(t, h, "", hubtest.ProjectOptions{Name: "Example project " + hubapi.NewID("fixture"), States: hubserver.HostedProjectStates()})
	hubfixture.ApprovePolicy(t, h, p.Base+"/policy", hubfixture.Policy())
	response := p.Request(t, http.MethodPost, p.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "landing"}, Title: "Land me", Body: hubfixture.IssueBody(), State: "Human Review"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	path := p.Base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	response = p.Request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Native change"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	l := landing{Change: hubfixture.Change{Native: p, Issue: issue, Change: change, Path: path + "/" + change.ID}}
	l.version = l.PublishSource(t, "publish", "", func(input *tracker.ChangeVersionInput) []byte { return sourcetest.Source(t, input) })
	hubfixture.RequireStatus(t, p.Request(t, http.MethodPost, l.Path+"/versions/"+l.version.ID+"/reviews", tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	if l.Issue = l.read(t); l.Issue.State != "Merging" {
		t.Fatalf("approved landing item is in %s", l.Issue.State)
	}
	l.runner = l.enroll(t, 2)
	return l
}

func (l landing) read(t *testing.T) tracker.NativeIssue {
	t.Helper()
	response := l.Request(t, http.MethodGet, l.Base+"/work-items/"+string(l.Issue.WorkItemID), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func (l landing) enroll(t *testing.T, capacity int) hubfixture.Runner {
	t.Helper()
	r := hubfixture.PrepareRunner(t, l.Native, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	r.Enroll(t)
	l.route(t, r, 1, capacity)
	r.ReportCapacity(t)
	return r
}

func (l landing) route(t *testing.T, r hubfixture.Runner, revision, capacity int) {
	t.Helper()
	hubfixture.RequireStatus(t, l.Hub.Request(t, http.MethodPut, r.IdentityPath()+"/routing", l.Hub.AdminToken(), map[string]any{"expected_revision": revision, "display_name": "Landing runner", "state": "active", "capacity_limit": capacity, "project_ids": []tracker.ProjectID{l.Project.ID}}), http.StatusOK)
}

func (l landing) claim(t *testing.T, r hubfixture.Runner, item tracker.NativeWorkItemID, session string) *httptest.ResponseRecorder {
	t.Helper()
	return l.Hub.Request(t, http.MethodPost, l.Base+"/claims", r.Redemption.Credential, tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: item, MachineID: r.Binding.MachineID, SessionID: session, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}})
}

func (l landing) runtime(t *testing.T) tracker.NativeRuntimeEvidence {
	t.Helper()
	response := l.Request(t, http.MethodGet, l.Base+"/work-items/"+string(l.Issue.WorkItemID)+"/runtime", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var evidence tracker.NativeRuntimeEvidence
	hubfixture.Decode(t, response, &evidence)
	return evidence
}

func (l landing) decisions(t *testing.T, item tracker.NativeWorkItemID) []tracker.CollaborationEvent {
	t.Helper()
	var rows []eventstore.Event
	if err := l.Hub.Repository.ReadTenant(t.Context(), l.Organization(), corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.Events(t.Context(), l.Project.ID, item, 0, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 100 {
		t.Fatal("decision history exceeds the complete read bound")
	}
	var result []tracker.CollaborationEvent
	for _, row := range rows {
		if row.Type != "scheduler.decision" {
			continue
		}
		event, err := eventstore.NewContent(l.Hub.Config.RunDetails).Event(t.Context(), row)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}

func claimed(events []tracker.CollaborationEvent) []tracker.CollaborationEvent {
	var result []tracker.CollaborationEvent
	for _, event := range events {
		if decision := event.Data.Decision; decision != nil && decision.Source == "native_claim" && decision.Outcome == "claimed" {
			result = append(result, event)
		}
	}
	return result
}

func (l landing) evaluated() tracker.NativeChangeReference {
	return tracker.NativeChangeReference{ChangeID: l.Change.Change.ID, VersionID: l.version.ID, HeadSHA: l.version.HeadSHA}
}

func requireClaimedLanding(t *testing.T, event tracker.CollaborationEvent, runner string, revision tracker.Revision, want tracker.NativeChangeReference) {
	t.Helper()
	decision := event.Data.Decision
	if decision == nil || decision.Source != "native_claim" || decision.Outcome != "claimed" || decision.RunnerID != runner || decision.WorkItemRevision != revision || event.Data.Change == nil || *event.Data.Change != want {
		t.Fatalf("claimed landing decision=%+v change=%+v, want runner %s revision %d change %+v", decision, event.Data.Change, runner, revision, want)
	}
}

func TestLandingClaimRecordsEvaluatedChange(t *testing.T) {
	t.Parallel()
	details := &claimDetails{DetailStore: hubtest.NewDetails()}
	h := hubfixture.OpenWith(t, func(config *hubfixture.Config) { config.RunDetails = details })

	t.Run("refusal then claim", func(t *testing.T) {
		l := newLanding(t, h)
		l.route(t, l.runner, 2, 0)
		hubfixture.RequireStatus(t, l.claim(t, l.runner, l.Issue.WorkItemID, "landing-refusal"), http.StatusConflict)
		history := l.decisions(t, l.Issue.WorkItemID)
		if len(history) != 1 || history[0].Data.Decision.Outcome != "skipped" || len(claimed(history)) != 0 {
			t.Fatalf("refusal history=%+v", history)
		}
		l.route(t, l.runner, 3, 2)
		response := l.claim(t, l.runner, l.Issue.WorkItemID, "landing-refusal")
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		hubfixture.Decode(t, response, &lease)
		history = l.decisions(t, l.Issue.WorkItemID)
		events := claimed(history)
		if len(events) != 1 || len(history) != 2 || history[0].Data.Decision.Outcome != "skipped" {
			t.Fatalf("history after claim=%+v", history)
		}
		requireClaimedLanding(t, events[0], l.runner.Binding.RunnerID, l.Issue.Revision, l.evaluated())
		evidence := l.runtime(t)
		if evidence.LatestDecision == nil || evidence.LatestDecision.ID != events[0].ID || evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID {
			t.Fatalf("current runtime presents latest=%+v lease=%+v, want claimed event %s and lease %s", evidence.LatestDecision, evidence.CurrentLease, events[0].ID, lease.ID)
		}
		for _, admission := range evidence.Admission {
			if admission.Outcome == "skipped" && admission.ReasonCode == "runner_capacity" {
				t.Fatalf("current runtime presents the old refusal as the blocker: %+v", admission)
			}
		}
	})

	t.Run("concurrent claim and replay", func(t *testing.T) {
		l := newLanding(t, h)
		start := make(chan struct{})
		responses := make([]*httptest.ResponseRecorder, 3)
		var workers sync.WaitGroup
		for index := range 2 {
			workers.Go(func() {
				<-start
				responses[index] = l.claim(t, l.runner, l.Issue.WorkItemID, "landing-race")
			})
		}
		close(start)
		workers.Wait()
		responses[2] = l.claim(t, l.runner, l.Issue.WorkItemID, "landing-race")
		owners := map[tracker.LeaseID]bool{}
		for _, response := range responses {
			if response.Code == http.StatusConflict {
				continue
			}
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			owners[lease.ID] = true
		}
		events := claimed(l.decisions(t, l.Issue.WorkItemID))
		if len(owners) != 1 || len(events) != 1 {
			t.Fatalf("leases=%v claimed decisions=%d, want one of each", owners, len(events))
		}
		requireClaimedLanding(t, events[0], l.runner.Binding.RunnerID, l.Issue.Revision, l.evaluated())
	})

	t.Run("refused or rolled back claim", func(t *testing.T) {
		l := newLanding(t, h)
		if _, err := h.Admin.ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE organization_id=$1 AND id=$2", l.Organization(), l.runner.Binding.MachineID); err != nil {
			t.Fatal(err)
		}
		hubfixture.RequireStatus(t, l.claim(t, l.runner, l.Issue.WorkItemID, "landing-host"), http.StatusConflict)
		l.route(t, l.runner, 2, 0)
		hubfixture.RequireStatus(t, l.claim(t, l.runner, l.Issue.WorkItemID, "landing-runner"), http.StatusConflict)
		if events := claimed(l.decisions(t, l.Issue.WorkItemID)); len(events) != 0 {
			t.Fatalf("refused claims recorded %d claimed decisions", len(events))
		}
	})

	t.Run("version moves before commit", func(t *testing.T) {
		l := newLanding(t, h)
		var moved tracker.ChangeVersion
		details.mu.Lock()
		details.onClaim = func() {
			moved = l.PublishSource(t, "republish", l.version.ID, func(input *tracker.ChangeVersionInput) []byte { return sourcetest.Source(t, input) })
		}
		details.mu.Unlock()
		response := l.claim(t, l.runner, l.Issue.WorkItemID, "landing-moved")
		if moved.ID == "" || moved.ID == l.version.ID {
			t.Fatalf("version did not move between preparation and commit: %+v", moved)
		}
		events := claimed(l.decisions(t, l.Issue.WorkItemID))
		for _, event := range events {
			if event.Data.Change == nil || event.Data.Change.VersionID != moved.ID || event.Data.Change.HeadSHA != moved.HeadSHA {
				t.Fatalf("claim recorded an evaluation of a replaced version: %+v (status %d)", event.Data.Change, response.Code)
			}
		}
		if response.Code == http.StatusOK && len(events) != 1 || response.Code != http.StatusOK && len(events) != 0 {
			t.Fatalf("status=%d claimed decisions=%d: %s", response.Code, len(events), response.Body.Bytes())
		}
	})

	t.Run("ordinary coding claim", func(t *testing.T) {
		l := newLanding(t, h)
		coding := l.Create(t, "Coding work")
		response := l.claim(t, l.runner, coding.WorkItemID, "coding")
		hubfixture.RequireStatus(t, response, http.StatusOK)
		events := claimed(l.decisions(t, coding.WorkItemID))
		if len(events) != 1 || events[0].Data.Change != nil || events[0].Data.Decision.RunnerID != l.runner.Binding.RunnerID {
			t.Fatalf("coding claim decisions=%+v", events)
		}
	})

	t.Run("another runner refused while the lease is active", func(t *testing.T) {
		l := newLanding(t, h)
		response := l.claim(t, l.runner, l.Issue.WorkItemID, "landing-owner")
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		hubfixture.Decode(t, response, &lease)
		other := l.enroll(t, 2)
		refused := l.claim(t, other, l.Issue.WorkItemID, "landing-other")
		hubfixture.RequireStatus(t, refused, http.StatusConflict)
		history := l.decisions(t, l.Issue.WorkItemID)
		events := claimed(history)
		if len(events) != 1 {
			t.Fatalf("claimed decisions=%d, want 1", len(events))
		}
		requireClaimedLanding(t, events[0], l.runner.Binding.RunnerID, l.Issue.Revision, l.evaluated())
		evidence := l.runtime(t)
		if evidence.CurrentLease == nil || evidence.CurrentLease.ID != lease.ID || evidence.LatestDecision == nil {
			t.Fatalf("runtime ownership lease=%+v latest=%+v", evidence.CurrentLease, evidence.LatestDecision)
		}
		latest := evidence.LatestDecision.Data.Decision
		if latest.RunnerID != other.Binding.RunnerID || latest.Outcome != "skipped" {
			t.Fatalf("latest decision=%+v, want the other runner's refusal beside the owner's lease", latest)
		}
	})
}
