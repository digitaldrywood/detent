package orchestrator

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestIssueContractDispatchAndRecovery(t *testing.T) {
	t.Parallel()
	complete := "## Acceptance criteria\nFix the endpoint.\n## Must not break\nExisting requests.\n## How we know it worked\nRun the endpoint tests."
	for _, test := range []struct {
		name      string
		lane      string
		target    string
		native    bool
		body      string
		exempt    bool
		confirmed bool
		blocked   bool
		preview   bool
		restart   bool
		delegated bool
		deferred  bool
	}{
		{name: "missing", blocked: true},
		{name: "native Triage exempt", lane: "Triage", target: "Triage", native: true},
		{name: "external active Triage", lane: "Triage", target: "Triage", blocked: true},
		{name: "external active Triage scheduler preview", lane: "Triage", target: "Triage", blocked: true, preview: true},
		{name: "Reporter promoted without engineering sections", body: "## Actual\nSaving fails.", blocked: true},
		{name: "Reporter promoted with engineering sections", body: "## Actual\nSaving fails.\n" + complete, confirmed: true},
		{name: "scheduler preview places the hold before claiming", blocked: true, preview: true},
		{name: "configured failure lane recovers after restart", blocked: true, restart: true},
		{name: "human owned", body: complete, confirmed: true},
		{name: "machine needs confirmation", body: complete, blocked: true},
		{name: "delegated complete machine criteria dispatch", body: complete, delegated: true},
		{name: "delegated incomplete Backlog stays with author", lane: "Backlog", delegated: true, deferred: true},
		{name: "rollout exemption", exempt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC)
			contract, err := workflowconfig.ResolveIssueContract("Run `make check-land`.")
			if err != nil {
				t.Fatal(err)
			}
			if test.delegated {
				contract.Context = "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n\nAgent-authored criteria do not require human confirmation.\n"
			}
			issue := connector.NewIssue()
			issue.ID, issue.Identifier, issue.Title, issue.State = "issue", "prj_contract#1", "Fix endpoint", "Todo"
			if test.lane != "" {
				issue.State = test.lane
			}
			if test.native {
				issue.Metadata["hub_profile"] = "native"
			}
			issue.Description = test.body
			issue.IssueContract = &issuecontract.State{Exempt: test.exempt}
			if test.confirmed {
				issue.IssueContract.ConfirmedSections = workflowconfig.IssueContractSectionDigests(issue.Description)
			}
			backend := fakehub.New(fakehub.Config{Stateful: true, Issues: []connector.Issue{issue}, Now: func() time.Time { return now }})
			cfg := laneMutationTestConfig()
			cfg.IssueContract = &contract
			cfg.StopRunTargetState = "Blocked"
			if test.target != "" {
				cfg.AdmissionTargetState = test.target
				if !test.native {
					cfg.ActiveStates = append(cfg.ActiveStates, test.target)
				}
			}
			if test.restart {
				cfg.StopRunTargetState = "Needs criteria"
				cfg.ObservedStates = append(cfg.ObservedStates, cfg.StopRunTargetState)
			}
			cfg.SchedulingRepository = "acme/widgets"
			state := newState(cfg)
			orch := &Orchestrator{cfg: cfg, connector: backend}
			if test.restart {
				db, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				orch.workflowMetrics = db
				orch.laneLedger = db
			}
			if test.deferred {
				allowed, err := orch.enforceIssueContract(t.Context(), &state, issue, "Todo", now)
				if err != nil || allowed || len(state.Blocked) != 0 {
					t.Fatalf("incomplete authoring gained a dispatch or human hold: allowed=%t blocked=%v err=%v", allowed, state.Blocked, err)
				}
				return
			}
			var candidates []connector.Issue
			if test.preview {
				source := &hubSchedulingSource{}
				orch.scheduling = source
				orch.now = func() time.Time { return now }
				if _, err := orch.fetchCandidateIssuesForTick(t.Context(), &state); err != nil {
					t.Fatal(err)
				}
				if source.request.CandidateReady(t.Context(), issue) {
					candidates = []connector.Issue{issue}
				}
			} else {
				candidates = orch.filterIssueContracts(t.Context(), &state, []connector.Issue{issue}, now)
			}
			if (len(candidates) == 0) != test.blocked {
				t.Fatalf("candidates = %v, blocked = %t", candidates, test.blocked)
			}
			if !test.blocked {
				if test.native {
					allowed, err := orch.enforceIssueContract(t.Context(), &state, issue, issue.State, now)
					if err != nil || !allowed {
						t.Fatalf("native Triage contract=%t, %v", allowed, err)
					}
				}
				if len(state.Blocked) != 0 {
					t.Fatalf("unexpected contract hold: %+v", state.Blocked)
				}
				return
			}
			blocked, found := state.Blocked[issue.ID]
			if !found || blocked.Issue.State != cfg.StopRunTargetState || blocked.Issue.WorkpadSignal.HumanAction == "" {
				t.Fatalf("missing human-action placement: %+v", blocked)
			}
			if test.body == "" && (!strings.Contains(blocked.Issue.WorkpadSignal.HumanAction, "## Acceptance criteria") || !strings.Contains(blocked.Issue.WorkpadSignal.HumanAction, "make check-land")) {
				t.Fatalf("missing drafted fix: %s", blocked.Issue.WorkpadSignal.HumanAction)
			}
			issue = blocked.Issue
			issue.Description = complete
			issue.Metadata["hub_profile"] = "native"
			issue.Metadata["hub_disposition_return_state"] = firstNonBlank(test.target, "Todo")
			issue.StageUpdatedAt = &now
			if test.restart {
				state = newState(cfg)
				issue.StageUpdatedAt = nil
			}
			later := now.Add(time.Minute)
			issue.IssueContract.ConfirmedSections = workflowconfig.IssueContractSectionDigests(complete)
			issue.IssueContract.RecordedAt = later
			issue.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusInProgress, RecordedAt: &later}
			if orch.recoverCauseBlockedIssue(t.Context(), &state, issue, later) {
				t.Fatal("recovered before the human action was cleared")
			}
			issue.IssueContract.HumanAction = ""
			if !orch.recoverCauseBlockedIssue(t.Context(), &state, issue, later) {
				t.Fatal("human confirmation did not recover through recorded-blocker recovery")
			}
		})
	}
}
