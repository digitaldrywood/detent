package scenariotest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func OpenPlannerAdmission(t *testing.T, factories Constructors, reports func() ([]providercapacity.Report, error)) (policy.Descriptor, *Scheduler, *Scheduler, func(string) string, *NativeClient) {
	t.Helper()
	h := newPlannerHub(t, "Human Review", hubserverPlanStates(), factories)
	previous := h.descriptor.ID
	h.descriptor.Gates.PlanEnabled, h.descriptor.Gates.PlanReview = true, gate.PlanReviewAutomated
	h.descriptor = h.descriptor.WithID()
	if _, err := h.admin.ApproveProjectPolicy(t.Context(), policy.Change{ExpectedID: previous, Policy: h.descriptor}); err != nil {
		t.Fatal(err)
	}
	base, _ := schedulerFixtures.Load(h.scheduler)
	cfg := base.(*schedulerFixture).config
	cfg.ProviderReports = reports
	local, err := newTestScheduler(h.native.Transport(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	competitor := h.otherWorker(t)
	identity, err := competitor.Transport().RunnerIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Machine.ID, cfg.Machine.Hostname = identity.MachineID, "competing-worker"
	other, err := newTestScheduler(competitor.Transport(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	create := func(title string) string {
		t.Helper()
		issue := createIndependentNativeIssue(t, h.admin, tracker.CreateIssue{Mutation: api.NativeMutationKey(), Title: title, Body: title + "." + issueContractTestSections, State: "Todo"})
		confirmIssueContract(t, h.admin, string(issue.WorkItemID))
		return string(issue.WorkItemID)
	}
	return h.descriptor, local, other, create, h.admin
}

func hubserverPlanStates() []tracker.NativeState {
	return []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Human Review", "Done"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Human Review", "Done"}},
		{Name: "Human Review", Transitions: []string{"In Progress", "Merging", "Done"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Human Review", "Done"}},
		{Name: "Done", Terminal: true},
	}
}

func assertSetupRefusalReceipt(t *testing.T, attempts []tracker.NativeAttempt) {
	t.Helper()
	if len(attempts) == 0 {
		t.Fatal("setup refusal receipt absent")
	}
	a := attempts[len(attempts)-1]
	if a.Identity != nil || len(a.Usage) != 0 || a.Status != "interrupted" || a.Checkpoint != nil || a.TerminalFailure == nil || a.TerminalFailure.Stage != "pre_provider" || a.TerminalFailure.Attribution != "instance" || a.TerminalFailure.ErrorClass != "source_recovery" || !strings.Contains(a.TerminalFailure.Error, "native checkpoint requires recovery:") {
		t.Fatalf("setup refusal replaced source or lost diagnostic: %+v failure=%+v", a, a.TerminalFailure)
	}
}

func assertInstanceRecoveryRetry(t *testing.T, h *nativeChangeHub, orch *orchestrator.Orchestrator, issueID string) {
	t.Helper()
	if candidates := h.candidates(t); len(candidates) != 1 || candidates[0].ID != issueID {
		t.Errorf("instance recovery refusal not reclaimable in its lane: candidates=%d", len(candidates))
	}
	testenv.WaitUntil(t, t.Context(), func() error {
		state, err := orch.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, retried := state.Retry[issueID]; !retried {
			return errors.New("recovery refusal retry is not published yet")
		}
		return nil
	})
	state, err := orch.State(t.Context())
	retry, retried := state.Retry[issueID]
	if err != nil || len(state.Retry) != 1 || !retried || !strings.Contains(retry.Error, "native checkpoint requires recovery") {
		t.Errorf("instance recovery refusal charged failures or skipped its retry: retry=%d error=%v", len(state.Retry), err)
	}
}

func newPlannerHub(t *testing.T, review string, states []tracker.NativeState, factories Constructors, repositoryBackend ...hubserver.ReconcileBackend) *nativeChangeHub {
	t.Helper()
	return newNativeChangeHubConfigured(t, review, states, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &hubserver.ConversationConfig{Enabled: true}}, factories, repositoryBackend...)
}

func nativeChangeDiff(head string, files ...string) runner.AttemptDiffSource {
	return func(context.Context) (tracker.AttemptDiffRequest, bool) {
		request := tracker.AttemptDiffRequest{BaseSHA: nativeChangeFixtureHead("a"), HeadSHA: head, Files: []tracker.AttemptDiffFile{}}
		for _, file := range files {
			request.Files = append(request.Files, tracker.AttemptDiffFile{Path: file, Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ -1 +1 @@"})
		}
		return request, true
	}
}
