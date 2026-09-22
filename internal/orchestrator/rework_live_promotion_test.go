package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func TestReworkLivePullRequestPromotion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		change      func(*connector.Issue)
		running     bool
		completed   bool
		wantPromote bool
	}{
		{name: "clean without completion", wantPromote: true},
		{name: "head newer than stale workpad", wantPromote: true, change: func(i *connector.Issue) {
			recorded := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
			head := recorded.Add(time.Minute)
			i.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil), UpdatedAt: &recorded}}
			i.PullRequest.HeadCommittedAt = &head
		}},
		{name: "clean with completion", completed: true, wantPromote: true},
		{name: "unresolved thread", change: func(i *connector.Issue) {
			i.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}}
		}},
		{name: "failing CI", change: func(i *connector.Issue) { i.PullRequest.CIStatus = "failure" }},
		{name: "unknown mergeability", change: func(i *connector.Issue) { i.PullRequest.MergeableState = "unknown" }},
		{name: "running worker", running: true},
		{name: "unfinished workpad", change: func(i *connector.Issue) {
			i.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil) + "\nImplementation remains outstanding."}}
		}},
		{name: "unfinished completed worker", completed: true, change: func(i *connector.Issue) {
			i.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil)}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}})
			issue := autoPromoteTickIssue("live-rework", nil, &connector.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "head", MergeableState: "CLEAN", CIStatus: "success"})
			issue.State = "Rework"
			if tt.change != nil {
				tt.change(&issue)
			}
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			if tt.completed {
				state.Completed[issue.ID] = Completed{FinalState: FinalStateCompleted}
			}
			if tt.running {
				state.Running[issue.ID] = Running{Issue: issue}
			}
			result := o.autoPromoteHumanReviewIssues(context.Background(), &state, []connector.Issue{issue}, time.Now())
			_, promoted := result.transitioned[issue.ID]
			if promoted != tt.wantPromote {
				t.Fatalf("promoted = %v, want %v; decisions = %#v", promoted, tt.wantPromote, state.AutoPromoteDecisions)
			}
		})
	}
}

func TestReworkLiveDraftPromotion(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		draft       bool
		wantForge   bool
		changeLive  func(*connector.Issue)
		wantPromote bool
	}{
		{name: "draft stays draft", draft: true},
		{name: "stale review draft stays draft", draft: true, changeLive: func(i *connector.Issue) {
			i.PullRequest.LatestCodexReviewState = "COMMENTED"
			i.PullRequest.LatestCodexReviewCommitSHA = "previous-head"
		}},
		{name: "draft with thread stays", draft: true, changeLive: func(i *connector.Issue) {
			i.PullRequest.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}}
		}},
		{name: "ready control", wantPromote: true},
		{name: "fresh unfinished workpad", changeLive: func(i *connector.Issue) {
			i.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil) + "\nStill implementing."}}
		}},
		{name: "fresh draft", changeLive: func(i *connector.Issue) { i.PullRequest.Draft = true }},
		{name: "fresh CI failure replaces cached green", changeLive: func(i *connector.Issue) { i.PullRequest.CIStatus = "failure" }},
		{name: "fresh required check failure", changeLive: func(i *connector.Issue) {
			i.PullRequest.RequiredCheckFailures = []connector.PullRequestCheck{{Name: "verify"}}
		}},
		{name: "fresh closed PR", changeLive: func(i *connector.Issue) { i.PullRequest.State = "CLOSED" }},
		{name: "fresh head lacks checks", changeLive: func(i *connector.Issue) { i.PullRequest.HeadSHA = "new-head"; i.PullRequest.CIStatus = "pending" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, GateWaitState: "review", Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}})
			issue := autoPromoteTickIssue("live-draft", nil, &connector.PullRequest{NodeID: "PR_42", Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "head", MergeableState: "CLEAN", CIStatus: "success", Draft: tt.draft})
			issue.State = "Rework"
			live := cloneIssue(issue)
			if tt.changeLive != nil {
				tt.changeLive(&live)
			}
			tracker := &liveReworkConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, live: live}
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			for tick := range 2 {
				result := o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
				if tick == 0 && tt.draft && len(result.transitioned) != 0 {
					t.Fatal("draft promoted before ready state was re-read")
				}
			}
			condition, waiting := forgeCondition(&state, "github.com")
			if waiting != tt.wantForge || waiting && condition.ErrorClass != forgeavailability.ClassWorkerGitHubCredentialUnavailable {
				t.Fatalf("forge condition = %#v, waiting=%v, want %v", condition, waiting, tt.wantForge)
			}
			if len(state.Blocked) != 0 || len(state.RepeatedFailures) != 0 || len(state.InstantFailures) != 0 {
				t.Fatal("draft-ready failure attributed to issue")
			}
			promoted := len(tracker.updates) > 0
			if promoted != tt.wantPromote {
				t.Fatalf("promoted=%v, want %v", promoted, tt.wantPromote)
			}
		})
	}
}

type liveReworkConnector struct {
	*autoPromoteTickConnector
	live connector.Issue
}

func (c *liveReworkConnector) HydratePullRequest(context.Context, connector.Issue) (connector.Issue, error) {
	issue := cloneIssue(c.live)
	issue.PullRequest.UnresolvedReviewThreads = nil
	return issue, nil
}

func TestReworkLiveSecurityAudit(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		passState string
		audit     securityaudit.Evaluation
		want      AutoPromoteAction
	}{
		{name: "non-merging destination requires audit", passState: "Done", audit: securityaudit.Evaluation{Reason: securityaudit.ReasonMissing}, want: AutoPromoteActionAwaitReview},
		{name: "not run", audit: securityaudit.Evaluation{Reason: securityaudit.ReasonMissing}, want: AutoPromoteActionPromote},
		{name: "pass", audit: securityaudit.Evaluation{Allowed: true, Reason: securityaudit.ReasonReady}, want: AutoPromoteActionPromote},
		{name: "running", audit: securityaudit.Evaluation{Running: true}, want: AutoPromoteActionAwaitReview},
		{name: "failed", audit: securityaudit.Evaluation{Reason: securityaudit.ReasonFailed}, want: AutoPromoteActionRework},
		{name: "findings", audit: securityaudit.Evaluation{Reason: securityaudit.ReasonUnresolvedFindings}, want: AutoPromoteActionRework},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := autoPromoteTickIssue("audit-rework", nil, &connector.PullRequest{Number: 42, URL: "https://github.com/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "head", MergeableState: "clean", CIStatus: "success"})
			issue.State = "Rework"
			summary := AutoPromoteSummaryFromIssue(issue)
			summary.SecurityAudit = tt.audit
			cfg := AutoPromoteConfig{Enabled: true, PassState: tt.passState, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false), SecurityAudit: gate.SecurityAuditConfig{Enabled: true}}}
			got := EvaluateAutoPromote(issue, summary, cfg, time.Now())
			if got.Action != tt.want {
				t.Fatalf("decision = %#v, want %s", got, tt.want)
			}
		})
	}
}

func (c *liveReworkConnector) HydratePullRequestReviewThreads(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	issue = cloneIssue(issue)
	issue.PullRequest.UnresolvedReviewThreads = append([]connector.PullRequestReviewThread(nil), c.live.PullRequest.UnresolvedReviewThreads...)
	return issue, nil
}

func TestReworkArtifactPromotion(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"approved", "pending_review"} {
		t.Run(status, func(t *testing.T) {
			cfg := normalizeConfig(Config{DeliverableKind: "artifact", ActiveStates: []string{"Rework"}, AutoPromote: AutoPromoteConfig{Enabled: true, SourceState: "Rework", PassState: "Done", Gate: gate.Config{Kind: gate.KindArtifact, Artifact: gate.ArtifactConfig{StatusField: "render_status", PassStatuses: []string{"approved"}, WaitStatuses: []string{"pending_review"}}}}})
			issue := autoPromoteTickIssue("artifact-rework", nil, nil)
			issue.State = "Rework"
			issue.Fields = map[string]string{"render_status": status}
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
			o := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			result := o.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
			_, promoted := result.transitioned[issue.ID]
			if promoted != (status == "approved") {
				t.Fatalf("promoted=%v, status=%s", promoted, status)
			}
		})
	}
}
