package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestCompletionForgeEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	older, newer := now.Add(-time.Minute), now.Add(time.Minute)
	for _, tt := range []struct {
		name           string
		pr             *connector.PullRequest
		recorded       *time.Time
		wantUnfinished bool
	}{
		{"merged", &connector.PullRequest{State: "MERGED", HeadSHA: "head"}, &now, false},
		{"newer open head", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer}, &now, false},
		{"no pull request", nil, &now, true},
		{"current in progress", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &older}, &now, true},
		{"equal timestamp", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &now}, &now, true},
		{"unknown status time", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer}, nil, true},
		{"draft", &connector.PullRequest{State: "OPEN", HeadSHA: "head", HeadCommittedAt: &newer, Draft: true}, &now, true},
		{"closed unmerged", &connector.PullRequest{State: "CLOSED", HeadSHA: "head", HeadCommittedAt: &newer}, &now, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := connector.Issue{PullRequest: tt.pr, WorkpadSignal: &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusInProgress, RecordedAt: tt.recorded}}
			if got := completionWorkpadUnfinished(issue); got != tt.wantUnfinished {
				t.Fatalf("unfinished = %v, want %v", got, tt.wantUnfinished)
			}
			if issue.WorkpadSignal.Status != workpad.StatusInProgress {
				t.Fatal("mutated source assertion")
			}
		})
	}
}

func TestCompletionForgeEvidenceClassification(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	for _, tt := range []struct {
		name, prState string
		noPR          bool
		want          store.WorkAttemptTerminalState
	}{
		{"merged", "MERGED", false, store.WorkAttemptTerminalSuccess},
		{"newer open head", "OPEN", false, store.WorkAttemptTerminalSuccess},
		{"no pull request", "", true, store.WorkAttemptTerminalNoProgress},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := implementProgressIssue("new-head")
			issue.PullRequest.State = tt.prState
			issue.PullRequest.HeadCommittedAt = &now
			issue.PullRequest.MergeableState = "clean"
			issue.PullRequest.CIStatus = "success"
			issue.PullRequest.CheckRunCount = 1
			issue.PullRequest.DiffFingerprint = "same-diff"
			if tt.noPR {
				issue = implementProgressIssueWithoutPR()
			}
			issue.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil), UpdatedAt: &old}}
			before := cloneIssue(issue)
			if before.PullRequest != nil {
				before.PullRequest.HeadSHA = "old-head"
			}
			tracker := &implementProgressConnector{refreshed: issue, hydrated: issue}
			attempts := &implementProgressAttemptStore{history: []store.WorkAttempt{implementProgressHistoryAttempt(1, autoPromoteReworkSignature{PRNumber: 1070, HeadSHA: "old-head"}, store.WorkAttemptTerminalSuccess)}}
			cfg := normalizeConfig(Config{ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand}}})
			orch := &Orchestrator{cfg: cfg, connector: tracker, workAttempts: attempts}
			decision := orch.evaluateImplementCompletionProgress(t.Context(), Running{Issue: before, DispatchProgress: implementProgressArtifactSnapshot{PullRequestDiffFingerprint: "same-diff"}, WorkAttemptID: 42, StartedAt: old.Add(time.Minute), DiffStats: DiffStats{Status: "clean", HeadSHA: "new-head"}}, FinalStateCompleted, false)
			if decision.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s: %#v", decision.Outcome, tt.want, decision)
			}
			if !tt.noPR && decision.WorkpadStatus != workpad.StatusComplete {
				t.Fatalf("status = %q", decision.WorkpadStatus)
			}
			if tt.noPR && !completionWorkpadUnfinished(decision.Issue) {
				t.Fatal("no PR no longer continues implementation")
			}
		})
	}
}

func TestCompletionForgeEvidenceDispatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	for _, lane := range []string{"In Progress", "Rework"} {
		for _, tt := range []struct {
			name, prState string
			wantDispatch  bool
		}{
			{"merged", "MERGED", false},
			{"newer open head", "OPEN", false},
			{"no pull request", "", true},
		} {
			t.Run(lane+"/"+tt.name, func(t *testing.T) {
				cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}})
				issue := dispatchTestIssueWithPullRequest("forge-evidence", lane, tt.prState)
				if tt.prState == "" {
					issue.PullRequest = nil
					issue.PRNumber = nil
				} else {
					issue.PullRequest.HeadSHA = "new-head"
					issue.PullRequest.HeadCommittedAt = &now
					issue.PullRequest.CIStatus = "success"
				}
				issue.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil), UpdatedAt: &old}}
				tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
				orch := &Orchestrator{cfg: cfg, connector: tracker}
				state := newState(cfg)
				state.Completed[issue.ID] = Completed{Issue: issue, FinalState: FinalStateCompleted, CompletedAt: now, successfulAttemptPersisted: true}
				orch.reconcileStaleLinkedPullRequestIssues(t.Context(), &state, []connector.Issue{issue}, now)
				orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)
				for _, update := range tracker.updates {
					if update.issueID == issue.ID {
						issue.State = update.state
					}
				}
				var selected bool
				newDispatchPlanner(cfg).plan(&state, []connector.Issue{issue}, now, dispatchPlanHooks{decision: func(d dispatchPlanDecision) { selected = d.Selected }})
				if selected != tt.wantDispatch {
					t.Fatalf("dispatch = %v, want %v; state=%s updates=%#v", selected, tt.wantDispatch, issue.State, tracker.updates)
				}
				if !tt.wantDispatch && len(tracker.updates) == 0 {
					t.Fatal("finished PR did not leave implementation")
				}
			})
		}
	}
}
