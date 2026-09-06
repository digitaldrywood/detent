package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestRequiredGateReportsConfiguredEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		prepare func(*connector.Issue, *AutoPromoteSummary)
		state   string
		reason  string
	}{
		{name: "passed", state: "passed", reason: string(gate.ReasonReady)},
		{name: "audit missing", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) {
			s.SecurityAudit = securityaudit.Evaluation{Reason: securityaudit.ReasonMissing}
		}, state: "pending", reason: string(gate.ReasonSecurityAuditMissing)},
		{name: "audit failed", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) {
			s.SecurityAudit = securityaudit.Evaluation{RunID: 13, Reason: securityaudit.ReasonFailed}
		}, state: "failed", reason: string(gate.ReasonSecurityAuditFailed)},
		{name: "audit findings", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) {
			s.SecurityAudit = securityaudit.Evaluation{RunID: 13, Reason: securityaudit.ReasonUnresolvedFindings}
		}, state: "failed", reason: string(gate.ReasonSecurityAuditFindings)},
		{name: "dirty", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) { s.MergeableState = "dirty" }, state: "failed", reason: string(AutoPromoteReasonMergeConflicts)},
		{name: "review thread", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) {
			s.UnresolvedReviewThreads = []connector.PullRequestReviewThread{{}}
		}, state: "failed", reason: string(AutoPromoteReasonUnresolvedReviewThreads)},
		{name: "CI failed", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) { s.FailedChecks = []string{"Test"} }, state: "failed", reason: string(AutoPromoteReasonCINotGreen)},
		{name: "hydration unavailable", prepare: func(_ *connector.Issue, s *AutoPromoteSummary) {
			s.PullRequestHydrationUnavailableReason = "unavailable"
		}, state: "unavailable", reason: string(AutoPromoteReasonPullRequestHydrationUnavailable)},
		{name: "capability blocked", prepare: func(i *connector.Issue, _ *AutoPromoteSummary) {
			i.Comments = nil
			i.WorkpadSignal = &workpad.Signal{Source: workpad.SourceStructured, Status: workpad.StatusBlocked, HumanAction: "Restore worker-github-cli-auth."}
		}, state: "failed", reason: string(AutoPromoteReasonWorkpadBlocker)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := reworkGateWaitTestIssue(workpad.StatusComplete)
			summary := AutoPromoteSummaryFromIssue(issue)
			summary.SecurityAudit = securityaudit.Evaluation{Allowed: true, Reason: securityaudit.ReasonReady}
			if tt.prepare != nil {
				tt.prepare(&issue, &summary)
			}
			cfg := gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false), SecurityAudit: gate.SecurityAuditConfig{Enabled: true}}
			got := requiredGateFromSummary(issue, summary, AutoPromoteConfig{Gate: cfg}, time.Now())
			if got.State != tt.state || got.Reason != tt.reason || got.CIState != "success" || got.HeadSHA != issue.PullRequest.HeadSHA {
				t.Fatalf("required gate = %#v, want %s/%s with green CI and exact head", got, tt.state, tt.reason)
			}
			if tt.name == "capability blocked" && got.HumanAction == "" {
				t.Fatal("capability recovery requirement missing")
			}
		})
	}
}

func TestRequiredGateSnapshotBindsExactPullRequest(t *testing.T) {
	t.Parallel()
	issue := reworkGateWaitTestIssue(workpad.StatusComplete)
	for _, tt := range []struct {
		name   string
		change func(*telemetry.RequiredGate)
		want   bool
	}{
		{name: "exact head", want: true},
		{name: "old head", change: func(g *telemetry.RequiredGate) { g.HeadSHA = "old" }},
		{name: "old base", change: func(g *telemetry.RequiredGate) { g.BaseSHA = "old" }},
		{name: "different PR", change: func(g *telemetry.RequiredGate) { g.PRNumber++ }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			required := telemetry.RequiredGate{State: "failed", PRNumber: issue.PullRequest.Number, HeadSHA: issue.PullRequest.HeadSHA, BaseSHA: issue.PullRequest.BaseSHA, AuditRunID: 13}
			if tt.change != nil {
				tt.change(&required)
			}
			state := newState(normalizeConfig(Config{}))
			state.RequiredGates = map[string]telemetry.RequiredGate{issue.ID: required}
			snapshots := []telemetry.Issue{{ID: issue.ID}}
			state.applyAutoPromoteDecisionSnapshots(snapshots, []connector.Issue{issue}, time.Now())
			if (snapshots[0].RequiredGate != nil) != tt.want {
				t.Fatalf("snapshot = %#v, want evidence %t", snapshots[0], tt.want)
			}
		})
	}
}

func TestRequiredGateRetainsConfiguredPassiveWaits(t *testing.T) {
	t.Parallel()
	issue := reworkGateWaitTestIssue(workpad.StatusComplete)
	now := time.Date(2026, 9, 5, 19, 18, 8, 0, time.UTC)
	for _, tt := range []struct {
		name string
		cfg  AutoPromoteConfig
		want string
	}{
		{name: "review quiet window", cfg: AutoPromoteConfig{QuietDuration: time.Minute, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}, want: string(gate.ReasonAutomatedReviewNotQuiet)},
		{name: "validator missing", cfg: AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false), Validator: gate.ValidatorConfig{Enabled: true}}}, want: string(gate.ReasonValidatorMissing)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary := AutoPromoteSummaryFromIssue(issue)
			summary.LastActivityAt = &now
			got := requiredGateFromSummary(issue, summary, tt.cfg, now)
			if got.State != "pending" || got.Reason != tt.want {
				t.Fatalf("gate=%#v, want pending/%s", got, tt.want)
			}
			orch := &Orchestrator{cfg: Config{AutoPromote: tt.cfg}, connector: &implementProgressConnector{refreshed: issue}}
			state := newState(orch.cfg)
			orch.refreshRequiredGateEvidence(t.Context(), &state, []connector.Issue{issue})
			if state.RequiredGates[issue.ID].State == "passed" {
				t.Fatalf("passive wait passed: %#v", state.RequiredGates[issue.ID])
			}
		})
	}
}
