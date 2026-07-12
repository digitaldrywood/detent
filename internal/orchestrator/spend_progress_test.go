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
		limit            float64
		spend            store.IssueSpendSince
		history          []store.WorkAttempt
		accepted         bool
		acceptedReason   string
		wantBlock        bool
		wantSpendCalls   int
		wantHistoryCalls int
		wantSince        time.Time
	}{
		{name: "disabled avoids tracking", limit: 0, spend: store.IssueSpendSince{CostUSD: 100}, wantSpendCalls: 0, wantHistoryCalls: 0},
		{name: "normal three retry sessions stay below default", limit: 3, spend: store.IssueSpendSince{CostUSD: 2.7, Sessions: 3}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "below threshold", limit: 5, spend: store.IssueSpendSince{CostUSD: 4.99, Sessions: 3}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "at threshold", limit: 5, spend: store.IssueSpendSince{CostUSD: 5, Sessions: 4}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "metered blocks above threshold", billingMode: "metered", limit: 5, spend: store.IssueSpendSince{CostUSD: 5.01, Sessions: 4}, wantBlock: true, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "subscription keeps above-threshold spend advisory", billingMode: "subscription", limit: 5, spend: store.IssueSpendSince{CostUSD: 5.01, Sessions: 4}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "July telemetry replay", limit: 5, spend: store.IssueSpendSince{CostUSD: 6.75, Sessions: 5}, wantBlock: true, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: createdAt},
		{name: "old sessions reset after accepted change", limit: 5, spend: store.IssueSpendSince{CostUSD: 1.25, Sessions: 1}, history: []store.WorkAttempt{acceptedAttempt}, wantSpendCalls: 1, wantHistoryCalls: 1, wantSince: acceptedAt},
		{name: "current accepted change resets without spend lookup", limit: 5, spend: store.IssueSpendSince{CostUSD: 100}, accepted: true, acceptedReason: "signature_changed", wantSpendCalls: 0, wantHistoryCalls: 0, wantSince: base},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spend := &spendProgressStore{result: tt.spend}
			attempts := &implementProgressAttemptStore{history: tt.history}
			orch := &Orchestrator{
				cfg: Config{
					Project:                 scheduler.ProjectCandidate{ID: "detent"},
					BillingMode:             tt.billingMode,
					NoProgressSpendLimitUSD: tt.limit,
				},
				progressSpend: spend,
				workAttempts:  attempts,
			}
			issue := connector.Issue{ID: "issue-214", Identifier: "gopherguides/gopher-ai#214", CreatedAt: &createdAt}
			decision := orch.evaluateSpendProgress(context.Background(), Running{Issue: issue}, base, tt.accepted, tt.acceptedReason)

			if decision.Block != tt.wantBlock {
				t.Fatalf("Block = %t, want %t", decision.Block, tt.wantBlock)
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

func TestDispatchAcceptedStateChange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		running Running
		want    bool
	}{
		{name: "lane transition", running: Running{DispatchSourceState: "Todo", DispatchTargetState: "In Progress"}, want: true},
		{name: "same lane", running: Running{DispatchSourceState: "Rework", DispatchTargetState: "Rework"}},
		{name: "missing transition", running: Running{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, reason := dispatchAcceptedStateChange(tt.running)
			if got != tt.want {
				t.Fatalf("accepted = %t, want %t", got, tt.want)
			}
			if got && reason != "lane_transition" {
				t.Fatalf("reason = %q, want lane_transition", reason)
			}
			if !got && reason != "" {
				t.Fatalf("reason = %q, want empty", reason)
			}
		})
	}
}

func TestHandleRunResultTripsSpendProgressIndependentlyOfContentDetector(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC)
	createdAt := base.Add(-30 * time.Minute)
	issue := implementProgressIssueWithoutPR()
	issue.CreatedAt = &createdAt
	tracker := &implementProgressConnector{}
	attempts := &implementProgressAttemptStore{}
	cfg := normalizeConfig(Config{
		Project:                 scheduler.ProjectCandidate{ID: "gopher-ai"},
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
			Sessions:       5,
			FirstSessionAt: base.Add(-14 * time.Minute),
			LastSessionAt:  base,
		}},
	}
	state := newState(cfg)
	running := Running{
		Issue:         issue,
		Attempt:       5,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeImplement,
		StartedAt:     base.Add(-2 * time.Minute),
		DiffStats:     DiffStats{FilesChanged: 2, AddedLines: 10, RemovedLines: 3, Status: "dirty"},
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
	for _, want := range []string{"$6.75", "$5.00", "Shrink the task", "first tool action"} {
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
	if !ok || record.BlockReason != spendProgressReason || math.Abs(record.SpendUSD-6.75) > 0.000001 {
		t.Fatalf("spend metadata = %#v, ok=%t", record, ok)
	}
	if !state.PriorAttempts[issue.ID].ExplainBeforeRetry {
		t.Fatalf("PriorAttempts[%q] = %#v, want explain-before-retry", issue.ID, state.PriorAttempts[issue.ID])
	}
}

func TestSpendProgressPriorAttemptRestoresExplainBeforeRetry(t *testing.T) {
	t.Parallel()

	attempt := store.WorkAttempt{WorkerMetadataJSON: marshalWorkAttemptJSON(map[string]any{
		spendProgressMetadataKey: spendProgressRecord{
			SpendUSD:    7.25,
			Sessions:    6,
			LimitUSD:    5,
			BlockReason: spendProgressReason,
		},
	})}
	orch := &Orchestrator{
		cfg:          Config{Project: scheduler.ProjectCandidate{ID: "detent"}, NoProgressSpendLimitUSD: 5},
		workAttempts: &implementProgressAttemptStore{history: []store.WorkAttempt{attempt}},
	}
	prior, ok := orch.spendProgressPriorAttempt(context.Background(), connector.Issue{ID: "issue-1"})
	if !ok || !prior.ExplainBeforeRetry || prior.ObservedSpendUSD != 7.25 || prior.NoProgressSpendLimitUSD != 5 {
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
		{name: "missing spend store", attempts: &implementProgressAttemptStore{}, missing: true, want: "progress spend store unavailable"},
		{name: "history lookup failure", attempts: &implementProgressAttemptStore{historyErr: errors.New("history unavailable")}, spend: &spendProgressStore{}, want: "history unavailable"},
		{name: "spend lookup failure", attempts: &implementProgressAttemptStore{}, spend: &spendProgressStore{err: errors.New("spend unavailable")}, want: "spend unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			orch := &Orchestrator{
				cfg:          Config{NoProgressSpendLimitUSD: 5},
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
