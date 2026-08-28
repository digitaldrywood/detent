package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestEvaluateSpendProgress(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	createdAt := base.Add(-time.Hour)
	acceptedAt := base.Add(-20 * time.Minute)
	acceptedAttempt := store.WorkAttempt{
		CompletedAt: acceptedAt,
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			spendProgressMetadataKey: spendProgressRecord{
				AcceptedStateChange: true,
				AcceptedReason:      "signature_changed",
				LimitUSD:            5,
			},
		}),
	}

	tests := []struct {
		name             string
		billingMode      string
		tokenLimit       int64
		limit            float64
		spend            store.IssueSpendSince
		history          []store.WorkAttempt
		issue            connector.Issue
		effort           string
		accepted         bool
		acceptedReason   string
		wantBlock        bool
		wantBlockedBy    string
		wantAccepted     bool
		wantReason       string
		wantLimit        float64
		wantSpendCalls   int
		wantHistoryCalls int
		wantSince        time.Time
	}{
		{name: "disabled avoids tracking", limit: 0, spend: store.IssueSpendSince{CostUSD: 100}, wantLimit: 0, wantSpendCalls: 0, wantHistoryCalls: 0},
		{name: "normal three retry sessions stay below default", limit: 3, spend: store.IssueSpendSince{CostUSD: 2.7, Sessions: 3}, wantLimit: 3, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "below threshold", limit: 5, spend: store.IssueSpendSince{CostUSD: 4.99, Sessions: 3}, wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "at threshold", limit: 5, spend: store.IssueSpendSince{CostUSD: 5, Sessions: 4}, wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "metered blocks above threshold", billingMode: "metered", limit: 5, spend: store.IssueSpendSince{CostUSD: 5.01, Sessions: 4}, wantBlock: true, wantBlockedBy: "usd", wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "subscription leaves USD breaker inert", billingMode: "subscription", limit: 5, spend: store.IssueSpendSince{CostUSD: 5.01, Sessions: 4}},
		{name: "subscription token breaker blocks at threshold", billingMode: "subscription", tokenLimit: 25_000_000, limit: 5, spend: store.IssueSpendSince{CostUSD: 5.01, TotalTokens: 25_000_000, Sessions: 4}, wantBlock: true, wantBlockedBy: "tokens", wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "subscription token breaker stays below threshold", billingMode: "subscription", tokenLimit: 25_000_000, spend: store.IssueSpendSince{TotalTokens: 24_999_999, Sessions: 3}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "July telemetry replay", limit: 5, spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 5}, wantBlock: true, wantBlockedBy: "usd", wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "old sessions reset after accepted change", limit: 5, spend: store.IssueSpendSince{CostUSD: 1.25, Sessions: 1}, history: []store.WorkAttempt{acceptedAttempt}, wantLimit: 5, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: acceptedAt},
		{name: "current accepted change resets without spend lookup", limit: 5, spend: store.IssueSpendSince{CostUSD: 100}, accepted: true, acceptedReason: "signature_changed", wantAccepted: true, wantReason: "signature_changed", wantLimit: 5, wantSpendCalls: 0, wantHistoryCalls: 0, wantSince: base},
		{
			name:  "dirty to clean pull request resets spend",
			limit: 5,
			spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
			issue: spendProgressIssueWithPR("same-head", "clean", "failure"),
			history: []store.WorkAttempt{{
				CompletedAt: base.Add(-10 * time.Minute),
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
					spendProgressMetadataKey: map[string]any{
						"limit_usd":      5,
						"pr_fingerprint": map[string]any{"number": 214, "head_sha": "same-head", "mergeable_state": "dirty", "ci_status": "failure"},
					},
				}),
			}},
			wantAccepted:     true,
			wantReason:       "pull_request_mergeable",
			wantLimit:        5,
			wantHistoryCalls: 1,
			wantSince:        base,
		},
		{
			name:  "failing to passing pull request resets spend",
			limit: 5,
			spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
			issue: spendProgressIssueWithPR("same-head", "dirty", "pass"),
			history: []store.WorkAttempt{{
				CompletedAt: base.Add(-10 * time.Minute),
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
					spendProgressMetadataKey: map[string]any{
						"limit_usd":      5,
						"pr_fingerprint": map[string]any{"number": 214, "head_sha": "same-head", "mergeable_state": "dirty", "ci_status": "fail"},
					},
				}),
			}},
			wantAccepted:     true,
			wantReason:       "pull_request_ci_passing",
			wantLimit:        5,
			wantHistoryCalls: 1,
			wantSince:        base,
		},
		{
			name:  "new pull request head resets spend",
			limit: 5,
			spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
			issue: spendProgressIssueWithPR("new-head", "dirty", "failure"),
			history: []store.WorkAttempt{{
				CompletedAt: base.Add(-10 * time.Minute),
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
					spendProgressMetadataKey: map[string]any{
						"limit_usd":      5,
						"pr_fingerprint": map[string]any{"number": 214, "head_sha": "old-head", "mergeable_state": "dirty", "ci_status": "failure"},
					},
				}),
			}},
			wantAccepted:     true,
			wantReason:       "pull_request_head_changed",
			wantLimit:        5,
			wantHistoryCalls: 1,
			wantSince:        base,
		},
		{
			name:  "byte identical pull request still parks",
			limit: 5,
			spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
			issue: spendProgressIssueWithPR("same-head", "dirty", "failure"),
			history: []store.WorkAttempt{{
				CompletedAt: base.Add(-10 * time.Minute),
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
					spendProgressMetadataKey: map[string]any{
						"limit_usd":      5,
						"pr_fingerprint": map[string]any{"number": 214, "head_sha": "same-head", "mergeable_state": "dirty", "ci_status": "failure"},
					},
				}),
			}},
			wantBlock:        true,
			wantBlockedBy:    "usd",
			wantLimit:        5,
			wantSpendCalls:   1,
			wantHistoryCalls: 1,
			wantSince:        createdAt,
		},
		{name: "xhigh threshold allows one expensive session", limit: 3, effort: "xhigh", spend: store.IssueSpendSince{CostUSD: 17.99, Sessions: 1}, wantLimit: 18, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spend := &spendProgressStore{result: tt.spend}
			attempts := &implementProgressAttemptStore{history: tt.history}
			billingMode := tt.billingMode
			if billingMode == "" {
				billingMode = "metered"
			}
			orch := &Orchestrator{
				cfg: Config{
					Project:                 scheduler.ProjectCandidate{ID: "detent"},
					BillingMode:             billingMode,
					NoProgressTokenLimit:    tt.tokenLimit,
					NoProgressSpendLimitUSD: tt.limit,
				},
				progressSpend: spend,
				workAttempts:  attempts,
			}
			issue := tt.issue
			issue.ID = "issue-214"
			issue.Identifier = "gopherguides/gopher-ai#214"
			issue.CreatedAt = &createdAt
			running := Running{Issue: issue}
			running.RuntimeIdentity.ReasoningEffort.Value = tt.effort
			decision := orch.evaluateSpendProgress(context.Background(), running, base, tt.accepted, tt.acceptedReason)

			if decision.Block != tt.wantBlock {
				t.Fatalf("Block = %t, want %t", decision.Block, tt.wantBlock)
			}
			if decision.BlockedBy != tt.wantBlockedBy {
				t.Fatalf("BlockedBy = %q, want %q", decision.BlockedBy, tt.wantBlockedBy)
			}
			if decision.AcceptedStateChange != tt.wantAccepted {
				t.Fatalf("AcceptedStateChange = %t, want %t", decision.AcceptedStateChange, tt.wantAccepted)
			}
			if decision.AcceptedReason != tt.wantReason {
				t.Fatalf("AcceptedReason = %q, want %q", decision.AcceptedReason, tt.wantReason)
			}
			if math.Abs(decision.LimitUSD-tt.wantLimit) > 0.000001 {
				t.Fatalf("LimitUSD = %f, want %f", decision.LimitUSD, tt.wantLimit)
			}
			if spend.calls != tt.wantSpendCalls {
				t.Fatalf("spend calls = %d, want %d", spend.calls, tt.wantSpendCalls)
			}
			if attempts.historyCalls != tt.wantHistoryCalls {
				t.Fatalf("history calls = %d, want %d", attempts.historyCalls, tt.wantHistoryCalls)
			}
			if !decision.Since.Equal(tt.wantSince) {
				t.Fatalf("Since = %s, want %s", decision.Since, tt.wantSince)
			}
			if spend.calls == 1 && !spend.query.Since.Equal(tt.wantSince) {
				t.Fatalf("query since = %s, want %s", spend.query.Since, tt.wantSince)
			}
		})
	}
}

func TestSpendProgressBaselineIgnoresLaneTransitions(t *testing.T) {
	t.Parallel()

	createdAt := time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)
	stageUpdatedAt := createdAt.Add(30 * time.Minute)
	acceptedAt := createdAt.Add(45 * time.Minute)
	tests := []struct {
		name     string
		attempts []store.WorkAttempt
		want     time.Time
	}{
		{name: "lane transition only", want: createdAt},
		{
			name: "accepted work product progress",
			attempts: []store.WorkAttempt{{
				CompletedAt: acceptedAt,
				WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
					spendProgressMetadataKey: spendProgressRecord{AcceptedStateChange: true},
				}),
			}},
			want: acceptedAt,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue := connector.Issue{CreatedAt: &createdAt, StageUpdatedAt: &stageUpdatedAt}
			if got := spendProgressBaseline(issue, tt.attempts); !got.Equal(tt.want) {
				t.Fatalf("spendProgressBaseline() = %s, want %s", got, tt.want)
			}
		})
	}
}

func spendProgressIssueWithPR(headSHA string, mergeableState string, ciStatus string) connector.Issue {
	number := 214
	return connector.Issue{
		PRNumber: &number,
		PullRequest: &connector.PullRequest{
			Number:         214,
			HeadSHA:        headSHA,
			MergeableState: mergeableState,
			CIStatus:       ciStatus,
		},
	}
}

func TestSpendProgressCommentNamesEvidenceCase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		decision     spendProgressDecision
		wantContains []string
	}{
		{
			name: "without PR evidence",
			decision: spendProgressDecision{
				Spend:     store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
				LimitUSD:  5,
				Case:      spendProgressCaseNoPR,
				BlockedBy: "usd",
			},
			wantContains: []string{"resource consumption continued without any PR evidence", "case: spend_without_pr_evidence", "Shrink the task"},
		},
		{
			name: "static PR evidence",
			decision: spendProgressDecision{
				Spend:         store.IssueSpendSince{CostUSD: 6.75, Sessions: 2},
				LimitUSD:      5,
				Case:          spendProgressCaseStatic,
				BlockedBy:     "usd",
				PRFingerprint: &spendProgressPRFingerprint{Number: 214, HeadSHA: "same-head", MergeableState: "dirty", CIStatus: "failure"},
			},
			wantContains: []string{"resource consumption continued while a linked PR existed but could not merge", "case: spend_with_static_pr_evidence", "merge-train capacity", "pr_head_sha: same-head"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			comment := spendProgressComment(connector.Issue{Identifier: "digitaldrywood/detent#1276"}, tt.decision)
			for _, want := range tt.wantContains {
				if !strings.Contains(comment, want) {
					t.Fatalf("comment missing %q:\n%s", want, comment)
				}
			}
			if recovery := spendProgressRecoveryReason(tt.decision); recovery == "" {
				t.Fatal("recovery reason is empty")
			}
			if handoff := spendProgressRetryHandoff(tt.decision); handoff.MissingSignal == "" {
				t.Fatal("retry handoff missing signal")
			}
		})
	}
}

func TestRefreshSpendProgressIssue(t *testing.T) {
	t.Parallel()

	baseIssue := connector.Issue{ID: "issue-1276", Identifier: "digitaldrywood/detent#1276"}
	linkedIssue := spendProgressIssueWithPR("head", "dirty", "failure")
	linkedIssue.ID = baseIssue.ID
	linkedIssue.Identifier = baseIssue.Identifier
	degradedIssue := cloneIssue(linkedIssue)
	degradedIssue.PullRequest.HydrationDegradedReason = connector.PullRequestHydrationReasonStaleCachedPullData

	tests := []struct {
		name        string
		orch        *Orchestrator
		issue       connector.Issue
		wantWarning string
		wantPR      bool
		wantHead    string
	}{
		{name: "missing connector", orch: &Orchestrator{}, issue: baseIssue, wantWarning: "refresh unavailable"},
		{name: "refresh failure", orch: &Orchestrator{connector: &implementProgressConnector{refreshErr: errors.New("github unavailable")}}, issue: baseIssue, wantWarning: "refresh failed"},
		{name: "refresh confirms no PR", orch: &Orchestrator{connector: &implementProgressConnector{refreshed: baseIssue}}, issue: baseIssue},
		{
			name: "refresh discovers and hydrates PR",
			orch: &Orchestrator{connector: &implementProgressConnector{
				refreshed: linkedIssue,
				hydrated:  linkedIssue,
			}},
			issue:    baseIssue,
			wantPR:   true,
			wantHead: "head",
		},
		{
			name:        "linked PR without hydrator",
			orch:        &Orchestrator{connector: connectorOnly{Connector: &implementProgressConnector{}}},
			issue:       linkedIssue,
			wantWarning: "hydrator unavailable",
			wantPR:      true,
			wantHead:    "head",
		},
		{
			name:        "hydration failure",
			orch:        &Orchestrator{connector: &implementProgressConnector{hydrateErr: errors.New("github unavailable")}},
			issue:       linkedIssue,
			wantWarning: "hydration failed",
			wantPR:      true,
			wantHead:    "head",
		},
		{
			name:        "degraded hydration",
			orch:        &Orchestrator{connector: &implementProgressConnector{hydrated: degradedIssue}},
			issue:       linkedIssue,
			wantWarning: connector.PullRequestHydrationReasonStaleCachedPullData,
			wantPR:      true,
			wantHead:    "head",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			issue, warning := tt.orch.refreshSpendProgressIssue(t.Context(), tt.issue)
			if !strings.Contains(warning, tt.wantWarning) {
				t.Fatalf("warning = %q, want containing %q", warning, tt.wantWarning)
			}
			if got := issue.PullRequest != nil; got != tt.wantPR {
				t.Fatalf("PR present = %t, want %t", got, tt.wantPR)
			}
			if tt.wantHead != "" && issue.PullRequest.HeadSHA != tt.wantHead {
				t.Fatalf("head = %q, want %q", issue.PullRequest.HeadSHA, tt.wantHead)
			}
		})
	}
}

type connectorOnly struct {
	connector.Connector
}

func TestImplementAcceptedStateChangeRequiresWorkProductProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		running    Running
		decision   implementCompletionProgressDecision
		want       bool
		wantReason string
	}{
		{name: "retry lane transition", running: Running{DispatchSourceState: "Todo", DispatchTargetState: "In Progress"}},
		{name: "rework lane transition", running: Running{DispatchSourceState: "Rework", DispatchTargetState: "In Progress"}},
		{name: "park lane transition", running: Running{DispatchSourceState: "In Progress", DispatchTargetState: "Blocked"}},
		{name: "pull request update", decision: implementCompletionProgressDecision{Reason: "pull_request_created_or_updated"}, want: true, wantReason: "pull_request_created_or_updated"},
		{name: "signature change", decision: implementCompletionProgressDecision{Reason: "signature_changed"}, want: true, wantReason: "signature_changed"},
		{name: "merged completion", decision: implementCompletionProgressDecision{Reason: implementMergedCompletionReason}, want: true, wantReason: implementMergedCompletionReason},
		{name: "operational reason without accepted kind", decision: implementCompletionProgressDecision{Reason: implementOperationalCompletion}},
		{name: "operational completion", decision: implementCompletionProgressDecision{Reason: string(AutoPromoteReasonOperationalCompletion), CompletionKind: workpad.CompletionOperational}, want: true, wantReason: string(AutoPromoteReasonOperationalCompletion)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := implementAcceptedStateChange(tt.running, tt.decision)
			if got != tt.want {
				t.Fatalf("accepted = %t, want %t", got, tt.want)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestOperationalCompletionSpendBreakerContract(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name            string
		authorized      bool
		wantTerminal    store.WorkAttemptTerminalState
		wantState       string
		wantBlocked     bool
		wantAccepted    bool
		wantCompletion  string
		wantSpendCase   string
		wantAcceptedWhy string
	}{
		{
			name:            "preauthorized operational completion bypasses no PR breaker",
			authorized:      true,
			wantTerminal:    store.WorkAttemptTerminalSuccess,
			wantState:       "Done",
			wantAccepted:    true,
			wantCompletion:  workpad.CompletionOperational,
			wantAcceptedWhy: string(AutoPromoteReasonOperationalCompletion),
		},
		{
			name:          "undeclared operational assertion trips existing breaker",
			wantTerminal:  store.WorkAttemptTerminalNoProgress,
			wantState:     blockedStatusState,
			wantBlocked:   true,
			wantSpendCase: spendProgressCaseNoPR,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			createdAt := base.Add(-time.Hour)
			recordedAt := base.Add(-time.Minute)
			issue := implementProgressIssueWithoutPR()
			issue.ID = "issue-operational"
			issue.Identifier = "digitaldrywood/leadpipe#62"
			issue.CreatedAt = &createdAt
			if tt.authorized {
				issue.Description = operationalCompletionAuthorizationBody()
			}
			issue.Comments = []connector.IssueComment{{Body: implementProgressStructuredWorkpad("in_progress", "", nil)}}
			refreshed := cloneIssue(issue)
			refreshed.Comments = []connector.IssueComment{{
				Body:      operationalCompletionWorkpadBody("Classifier-v4 host backfill completed and verified."),
				CreatedAt: &recordedAt,
			}}
			tracker := &implementProgressConnector{refreshed: refreshed}
			attempts := &implementProgressAttemptStore{}
			cfg := normalizeConfig(Config{
				Project:              scheduler.ProjectCandidate{ID: "leadpipe"},
				BillingMode:          "subscription",
				NoProgressTokenLimit: 25_000_000,
				AutoPromote: AutoPromoteConfig{
					Enabled:         true,
					NoProgressLimit: 0,
				},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				ObservedStates: []string{"Human Review", "Blocked"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			orch := &Orchestrator{
				cfg:          cfg,
				connector:    tracker,
				workAttempts: attempts,
				progressSpend: &spendProgressStore{result: store.IssueSpendSince{
					TotalTokens: 25_604_036,
					Sessions:    3,
				}},
			}
			state := newState(cfg)
			running := Running{
				Issue:            issue,
				Attempt:          3,
				WorkAttemptID:    7187,
				Mode:             runpkg.RunModeImplement,
				StartedAt:        base.Add(-time.Minute),
				DiffStats:        DiffStats{Status: "clean"},
				DispatchProgress: implementProgressArtifactSnapshotFromIssue(issue, true),
			}
			state.Running[issue.ID] = running
			state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: running.StartedAt}

			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID:     issue.ID,
				CompletedAt: base,
				Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
				Result: runpkg.RunResult{
					FinalState: FinalStateCompleted,
					DiffStats:  DiffStats{Status: "clean"},
				},
			})
			if tt.authorized {
				orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{refreshed}, base.Add(time.Second))
			}

			if len(attempts.completions) != 1 {
				t.Fatalf("completions = %#v, want one", attempts.completions)
			}
			completion := attempts.completions[0]
			if completion.TerminalState != tt.wantTerminal {
				t.Fatalf("terminal state = %q, want %q", completion.TerminalState, tt.wantTerminal)
			}
			if len(tracker.updates) != 1 || tracker.updates[0].state != tt.wantState {
				t.Fatalf("updates = %#v, want state %q", tracker.updates, tt.wantState)
			}
			if _, blocked := state.Blocked[issue.ID]; blocked != tt.wantBlocked {
				t.Fatalf("blocked = %t, want %t", blocked, tt.wantBlocked)
			}
			progress := implementProgressRecordFromCompletion(t, completion)
			if progress.CompletionKind != tt.wantCompletion {
				t.Fatalf("completion kind = %q, want %q", progress.CompletionKind, tt.wantCompletion)
			}
			spend, ok := spendProgressRecordFromAttempt(store.WorkAttempt{WorkerMetadataJSON: completion.WorkerMetadataJSON})
			if !ok {
				t.Fatal("spend progress metadata missing")
			}
			if spend.AcceptedStateChange != tt.wantAccepted || spend.AcceptedReason != tt.wantAcceptedWhy || spend.Case != tt.wantSpendCase {
				t.Fatalf("spend progress = %#v", spend)
			}
		})
	}
}

func TestHandleRunResultTripsTokenProgressBreakerOnSubscription(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	createdAt := base.Add(-30 * time.Minute)
	issue := implementProgressIssueWithoutPR()
	issue.CreatedAt = &createdAt
	stageUpdatedAt := base.Add(-5 * time.Minute)
	issue.StageUpdatedAt = &stageUpdatedAt
	tracker := &implementProgressConnector{}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:                 scheduler.ProjectCandidate{ID: "gopher-ai"},
		BillingMode:             "subscription",
		NoProgressTokenLimit:    25_000_000,
		NoProgressSpendLimitUSD: 5,
		AutoPromote:             AutoPromoteConfig{NoProgressLimit: 0},
		ActiveStates:            []string{"Todo", "In Progress", "Rework"},
		ObservedStates:          []string{"Blocked"},
		TerminalStates:          []string{"Done"},
	})
	orch := &Orchestrator{
		cfg:          cfg,
		connector:    tracker,
		workAttempts: attempts,
		progressSpend: &spendProgressStore{result: store.IssueSpendSince{
			CostUSD:        6.75,
			TotalTokens:    25_000_000,
			Sessions:       5,
			FirstSessionAt: base.Add(-14 * time.Minute),
			LastSessionAt:  base,
		}},
	}
	state := newState(cfg)
	running := Running{
		Issue:               issue,
		Attempt:             5,
		WorkAttemptID:       42,
		Mode:                runpkg.RunModeImplement,
		StartedAt:           base.Add(-2 * time.Minute),
		DiffStats:           DiffStats{FilesChanged: 2, AddedLines: 10, RemovedLines: 3, Status: "dirty"},
		DispatchSourceState: "Rework",
		DispatchTargetState: "In Progress",
	}
	state.Running[issue.ID] = running
	state.Claimed[issue.ID] = Claimed{Issue: issue, ClaimedAt: running.StartedAt}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: base,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Result: runpkg.RunResult{
			FinalState: runpkg.FinalStateCompleted,
			DiffStats:  running.DiffStats,
		},
	})

	blocked, ok := state.Blocked[issue.ID]
	if !ok || blocked.Reason != spendProgressReason {
		t.Fatalf("Blocked[%q] = %#v, want spend breaker", issue.ID, blocked)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %#v, want one", tracker.comments)
	}
	for _, want := range []string{"blocked_by: tokens", "25000000", "usd_breaker: inert", "Shrink the task", "first tool action"} {
		if !strings.Contains(tracker.comments[0].body, want) {
			t.Fatalf("comment missing %q:\n%s", want, tracker.comments[0].body)
		}
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("completions = %#v, want one", attempts.completions)
	}
	completion := attempts.completions[0]
	if completion.TerminalState != store.WorkAttemptTerminalNoProgress || completion.ErrorClass != spendProgressReason {
		t.Fatalf("completion = %#v, want spend no-progress terminal", completion)
	}
	record, ok := spendProgressRecordFromAttempt(store.WorkAttempt{WorkerMetadataJSON: completion.WorkerMetadataJSON})
	if !ok || record.BlockReason != spendProgressReason || record.BlockedBy != "tokens" || record.TotalTokens != 25_000_000 {
		t.Fatalf("spend metadata = %#v, ok=%t", record, ok)
	}
	if !state.PriorAttempts[issue.ID].ExplainBeforeRetry {
		t.Fatalf("PriorAttempts[%q] = %#v, want explain-before-retry", issue.ID, state.PriorAttempts[issue.ID])
	}
}

func TestHandleRunResultAcceptsPRAdvanceBeforeWorkerError(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 16, 0, 0, 0, time.UTC)
	createdAt := base.Add(-time.Hour)
	runningIssue := spendProgressIssueWithPR("old-head", "dirty", "failure")
	runningIssue.ID = "issue-1276"
	runningIssue.Identifier = "digitaldrywood/detent#1276"
	runningIssue.State = "In Progress"
	runningIssue.CreatedAt = &createdAt
	hydratedIssue := cloneIssue(runningIssue)
	hydratedIssue.PullRequest.HeadSHA = "new-head"
	tracker := &implementProgressConnector{hydrated: hydratedIssue}
	attempts := &implementProgressAttemptStore{history: []store.WorkAttempt{{
		CompletedAt: base.Add(-20 * time.Minute),
		WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
			spendProgressMetadataKey: map[string]any{
				"limit_usd":      5,
				"pr_fingerprint": map[string]any{"number": 214, "head_sha": "old-head", "mergeable_state": "dirty", "ci_status": "failure"},
			},
		}),
	}}}
	cfg := normalizeConfig(Config{
		Project:                 scheduler.ProjectCandidate{ID: "detent"},
		BillingMode:             "metered",
		NoProgressSpendLimitUSD: 5,
		ActiveStates:            []string{"In Progress"},
		ObservedStates:          []string{"Blocked"},
		TerminalStates:          []string{"Done"},
	})
	orch := &Orchestrator{
		cfg:           cfg,
		connector:     tracker,
		workAttempts:  attempts,
		progressSpend: &spendProgressStore{result: store.IssueSpendSince{CostUSD: 6.75, Sessions: 2}},
	}
	state := newState(cfg)
	running := Running{
		Issue:               runningIssue,
		Attempt:             2,
		WorkAttemptID:       42,
		Mode:                runpkg.RunModeImplement,
		StartedAt:           base.Add(-5 * time.Minute),
		DispatchSourceState: "Rework",
		DispatchTargetState: "In Progress",
	}
	state.Running[runningIssue.ID] = running
	state.Claimed[runningIssue.ID] = Claimed{Issue: runningIssue, ClaimedAt: running.StartedAt}

	orch.handleRunResult(context.Background(), &state, runpkg.Completion{
		IssueID:     runningIssue.ID,
		CompletedAt: base,
		Request:     runpkg.RunRequest{Mode: runpkg.RunModeImplement},
		Err:         errors.New("session token ceiling exceeded"),
	})

	if _, blocked := state.Blocked[runningIssue.ID]; blocked {
		t.Fatalf("Blocked[%q] present after PR head advanced", runningIssue.ID)
	}
	if tracker.hydrations != 1 {
		t.Fatalf("hydrations = %d, want 1", tracker.hydrations)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("completions = %#v, want one", attempts.completions)
	}
	record, ok := spendProgressRecordFromAttempt(store.WorkAttempt{WorkerMetadataJSON: attempts.completions[0].WorkerMetadataJSON})
	if !ok || !record.AcceptedStateChange || record.AcceptedReason != "pull_request_head_changed" {
		t.Fatalf("spend metadata = %#v, ok=%t", record, ok)
	}
}

func TestSpendProgressUSDMessagesLabelNotionalValue(t *testing.T) {
	t.Parallel()

	decision := spendProgressDecision{
		BlockedBy: "usd",
		Spend:     store.IssueSpendSince{CostUSD: 6.75},
		LimitUSD:  5,
	}
	for name, value := range map[string]string{
		"block":   spendProgressBlockMessage(decision),
		"summary": spendProgressUsageSummary(decision),
	} {
		if !strings.Contains(value, "notional USD") {
			t.Fatalf("%s message = %q, want notional USD label", name, value)
		}
	}
}

func TestSpendProgressPriorAttemptRestoresExplainBeforeRetry(t *testing.T) {
	t.Parallel()

	attempt := store.WorkAttempt{WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
		spendProgressMetadataKey: spendProgressRecord{
			TotalTokens: 25_000_000,
			SpendUSD:    7.25,
			Sessions:    6,
			TokenLimit:  25_000_000,
			LimitUSD:    5,
			BlockedBy:   "tokens",
			BlockReason: spendProgressReason,
		},
	})}
	orch := &Orchestrator{
		cfg:          Config{Project: scheduler.ProjectCandidate{ID: "detent"}, BillingMode: "subscription", NoProgressTokenLimit: 25_000_000, NoProgressSpendLimitUSD: 5},
		workAttempts: &implementProgressAttemptStore{history: []store.WorkAttempt{attempt}},
	}
	prior, ok := orch.spendProgressPriorAttempt(context.Background(), connector.Issue{ID: "issue-1"})
	if !ok || !prior.ExplainBeforeRetry || prior.ObservedTokens != 25_000_000 || prior.NoProgressTokenLimit != 25_000_000 {
		t.Fatalf("prior = %#v, ok=%t", prior, ok)
	}
}

func TestEvaluateSpendProgressFailsOpen(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	issue := connector.Issue{ID: "issue-1"}
	tests := []struct {
		name     string
		attempts *implementProgressAttemptStore
		spend    *spendProgressStore
		missing  bool
		want     string
	}{
		{name: "missing spend store", attempts: &implementProgressAttemptStore{}, missing: true, want: "progress usage store unavailable"},
		{name: "history lookup failure", attempts: &implementProgressAttemptStore{historyErr: errors.New("history unavailable")}, spend: &spendProgressStore{}, want: "history unavailable"},
		{name: "spend lookup failure", attempts: &implementProgressAttemptStore{}, spend: &spendProgressStore{err: errors.New("spend unavailable")}, want: "spend unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			orch := &Orchestrator{
				cfg:          Config{NoProgressTokenLimit: 25_000_000},
				workAttempts: tt.attempts,
				logger:       slog.New(slog.NewTextHandler(&logs, nil)),
			}
			if !tt.missing {
				orch.progressSpend = tt.spend
			}
			decision := orch.evaluateSpendProgress(t.Context(), Running{Issue: issue}, base, false, "")
			if decision.Block || !strings.Contains(decision.Warning, tt.want) || !strings.Contains(logs.String(), tt.want) {
				t.Fatalf("decision = %#v logs = %q, want warning %q", decision, logs.String(), tt.want)
			}
		})
	}
}

func TestSpendProgressAttemptAcceptedCompatibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record map[string]any
		want   bool
	}{
		{name: "native accepted record", record: map[string]any{spendProgressMetadataKey: spendProgressRecord{AcceptedStateChange: true}}, want: true},
		{name: "legacy pull request change", record: map[string]any{implementProgressMetadataKey: implementProgressRecord{Outcome: string(store.WorkAttemptTerminalSuccess), Reason: "pull_request_created_or_updated"}}, want: true},
		{name: "legacy signature change", record: map[string]any{implementProgressMetadataKey: implementProgressRecord{Outcome: string(store.WorkAttemptTerminalSuccess), Reason: "signature_changed"}}, want: true},
		{name: "operational completion", record: map[string]any{implementProgressMetadataKey: implementProgressRecord{Outcome: string(store.WorkAttemptTerminalSuccess), Reason: implementOperationalCompletion, CompletionKind: workpad.CompletionOperational}}, want: true},
		{name: "operational reason without accepted kind", record: map[string]any{implementProgressMetadataKey: implementProgressRecord{Outcome: string(store.WorkAttemptTerminalSuccess), Reason: implementOperationalCompletion}}},
		{name: "unaccepted record", record: map[string]any{implementProgressMetadataKey: implementProgressRecord{Outcome: string(store.WorkAttemptTerminalSuccess), Reason: "unchanged_signature_clean_diff"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attempt := store.WorkAttempt{
				TerminalState:      store.WorkAttemptTerminalSuccess,
				WorkerMetadataJSON: marshalWorkAttemptJSON(tt.record),
			}
			if got := spendProgressAttemptAccepted(attempt); got != tt.want {
				t.Fatalf("spendProgressAttemptAccepted() = %t, want %t", got, tt.want)
			}
		})
	}
}

type spendProgressStore struct {
	result store.IssueSpendSince
	err    error
	query  store.IssueSpendSinceQuery
	calls  int
}

func (s *spendProgressStore) IssueSpendSince(_ context.Context, query store.IssueSpendSinceQuery) (store.IssueSpendSince, error) {
	s.calls++
	s.query = query
	return s.result, s.err
}
