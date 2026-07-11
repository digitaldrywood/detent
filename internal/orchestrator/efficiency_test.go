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

type efficiencyRecorderSpy struct {
	completion efficiency.Completion
	receipt    efficiency.Receipt
	calls      int
}

func (s *efficiencyRecorderSpy) CompleteEfficiencyReceipt(_ context.Context, completion efficiency.Completion) (efficiency.Receipt, error) {
	s.completion = completion
	s.calls++
	return s.receipt, nil
}

type lifecycleExporterSpy struct {
	receipt efficiency.Receipt
}

func (s *lifecycleExporterSpy) ExportLifecycle(_ context.Context, receipt efficiency.Receipt) error {
	s.receipt = receipt
	return nil
}
