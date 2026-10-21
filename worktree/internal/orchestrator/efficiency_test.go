package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/efficiency"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestRecordEfficiencyReceiptPersistsAndExports(t *testing.T) {
	t.Parallel()

	recorder := &efficiencyRecorderSpy{receipt: efficiency.Receipt{IssueID: "issue-1205", TotalTokens: 1200}}
	exporter := &lifecycleExporterSpy{}
	orch := &Orchestrator{
		cfg: Config{
			Project:              scheduler.ProjectCandidate{ID: "detent"},
			EfficiencyThresholds: efficiency.Thresholds{TokensMultiple: 2, SessionsMultiple: 3, DwellMultiple: 4},
		},
		efficiency:        recorder,
		lifecycleExporter: exporter,
	}
	completedAt := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	prNumber := int64(1300)
	orch.recordEfficiencyReceipt(context.Background(), connector.Issue{
		ID:          "issue-1205",
		Identifier:  "digitaldrywood/detent#1205",
		URL:         "https://github.com/digitaldrywood/detent/issues/1205",
		State:       "Done",
		PullRequest: &connector.PullRequest{Number: int(prNumber)},
	}, completedAt)

	if recorder.completion.ProjectID != "detent" || recorder.completion.IssueID != "issue-1205" || !recorder.completion.CompletedAt.Equal(completedAt) {
		t.Fatalf("completion = %#v", recorder.completion)
	}
	if recorder.completion.PRNumber == nil || *recorder.completion.PRNumber != prNumber {
		t.Fatalf("completion PR number = %#v, want %d", recorder.completion.PRNumber, prNumber)
	}
	if recorder.completion.Thresholds != orch.cfg.EfficiencyThresholds {
		t.Fatalf("completion thresholds = %#v, want %#v", recorder.completion.Thresholds, orch.cfg.EfficiencyThresholds)
	}
	if exporter.receipt.IssueID != recorder.receipt.IssueID || exporter.receipt.TotalTokens != recorder.receipt.TotalTokens {
		t.Fatalf("exported receipt = %#v, want %#v", exporter.receipt, recorder.receipt)
	}
	orch.recordEfficiencyReceipt(context.Background(), connector.Issue{ID: "issue-cancelled", State: "Cancelled"}, completedAt)
	if recorder.calls != 1 {
		t.Fatalf("recorder calls = %d, want cancelled outcome skipped", recorder.calls)
	}
}

func TestRefreshEfficiencyReceiptRecordsIncompleteIssue(t *testing.T) {
	t.Parallel()

	recorder := &efficiencyRecorderSpy{}
	orch := &Orchestrator{
		cfg: normalizeConfig(Config{
			Project:              scheduler.ProjectCandidate{ID: "detent"},
			TerminalStates:       []string{"Done"},
			EfficiencyThresholds: efficiency.Thresholds{TokensMultiple: 2, SessionsMultiple: 3, DwellMultiple: 4},
		}),
		efficiency: recorder,
	}
	observedAt := time.Date(2026, 8, 19, 20, 0, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:         "issue-1926",
		Identifier: "digitaldrywood/detent#1926",
		URL:        "https://github.com/digitaldrywood/detent/issues/1926",
		State:      "In Progress",
	}

	orch.refreshEfficiencyReceipt(t.Context(), issue, observedAt)

	if recorder.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", recorder.refreshCalls)
	}
	if recorder.observation.ProjectID != "detent" || recorder.observation.IssueID != issue.ID || !recorder.observation.ObservedAt.Equal(observedAt) {
		t.Fatalf("observation = %#v, want issue identity and observed time", recorder.observation)
	}
	if recorder.observation.RefreshIntervalSessions != 5 {
		t.Fatalf("refresh interval = %d, want 5", recorder.observation.RefreshIntervalSessions)
	}
	if recorder.observation.Thresholds != orch.cfg.EfficiencyThresholds {
		t.Fatalf("thresholds = %#v, want %#v", recorder.observation.Thresholds, orch.cfg.EfficiencyThresholds)
	}

	issue.State = "Done"
	orch.refreshEfficiencyReceipt(t.Context(), issue, observedAt)
	if recorder.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want terminal issue skipped", recorder.refreshCalls)
	}
}

type efficiencyRecorderSpy struct {
	completion   efficiency.Completion
	observation  efficiency.Observation
	receipt      efficiency.Receipt
	calls        int
	refreshCalls int
}

func (s *efficiencyRecorderSpy) CompleteEfficiencyReceipt(_ context.Context, completion efficiency.Completion) (efficiency.Receipt, error) {
	s.completion = completion
	s.calls++
	return s.receipt, nil
}

func (s *efficiencyRecorderSpy) RefreshEfficiencyReceipt(_ context.Context, observation efficiency.Observation) (efficiency.Receipt, bool, error) {
	s.observation = observation
	s.refreshCalls++
	return s.receipt, true, nil
}

type lifecycleExporterSpy struct {
	receipt efficiency.Receipt
}

func (s *lifecycleExporterSpy) ExportLifecycle(_ context.Context, receipt efficiency.Receipt) error {
	s.receipt = receipt
	return nil
}
