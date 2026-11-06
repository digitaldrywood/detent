package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

func (s *sqliteStore) populateDailyShippedCohort(ctx context.Context, day *DailyDigestDay, window DailyDigestWindow, outcomes []ShippedOutcome) error {
	day.ShippedByProject = map[string]int64{}
	var receipts []efficiency.Receipt
	for _, outcome := range outcomes {
		if window.ProjectIDs != nil && !slices.Contains(window.ProjectIDs, outcome.ProjectID) {
			continue
		}
		if outcome.CompletedAt.Before(window.From) || !outcome.CompletedAt.Before(window.To) {
			continue
		}
		day.IssuesShipped++
		day.ShippedByProject[outcome.ProjectID]++
		receipt, err := s.EfficiencyReceipt(ctx, outcome.ProjectID, outcome.IssueID, "")
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if receipt.InProgress || receipt.CompletedAt.Before(window.From) || !receipt.CompletedAt.Before(window.To) {
			continue
		}
		// Read lane evidence afresh: old receipts assign residual lifetime elapsed
		// time to working, so their normalized dwell is not measured lane history.
		dwell, unknown, err := s.dailyCohortDwell(ctx, receipt, outcome.CompletedAt)
		if err != nil {
			return err
		}
		receipt.WorkingSeconds = dwell.WorkingSeconds
		receipt.GateWaitSeconds = dwell.GateWaitSeconds
		receipt.MergeTrainSeconds = dwell.MergeTrainSeconds
		receipt.ParkedSeconds = dwell.ParkedSeconds
		day.UnknownDwellSeconds += unknown
		receipts = append(receipts, receipt)
	}
	day.Efficiency = efficiencyRollupWindow(receipts)
	return nil
}

func (s *sqliteStore) dailyCohortDwell(ctx context.Context, receipt efficiency.Receipt, completed time.Time) (efficiency.Dwell, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT phase_name, started_at, finished_at
 FROM workflow_phase_events INDEXED BY workflow_phase_events_issue_idx
 WHERE project_id = ? AND issue_id = ? AND phase_type = 'lane'
   AND status = 'exited' AND finished_at IS NOT NULL`, receipt.ProjectID, receipt.IssueID)
	if err != nil {
		return efficiency.Dwell{}, 0, err
	}
	defer rows.Close()
	var dwell efficiency.Dwell
	for rows.Next() {
		var lane, fromText, toText string
		if err := rows.Scan(&lane, &fromText, &toText); err != nil {
			return dwell, 0, err
		}
		from, err := parseStoredTime(fromText)
		if err != nil {
			return dwell, 0, err
		}
		to, err := parseStoredTime(toText)
		if err != nil {
			return dwell, 0, err
		}
		if from.Before(receipt.FirstDispatchedAt) {
			from = receipt.FirstDispatchedAt
		}
		if to.After(completed) {
			to = completed
		}
		seconds := nonNegativeDurationSeconds(from, to)
		switch strings.ToLower(strings.TrimSpace(lane)) {
		case "in progress", "rework", "todo":
			dwell.WorkingSeconds += seconds
		case "human review", "review", "gate wait":
			dwell.GateWaitSeconds += seconds
		case "merging":
			dwell.MergeTrainSeconds += seconds
		case "blocked":
			dwell.ParkedSeconds += seconds
		}
	}
	unknown := max(nonNegativeDurationSeconds(receipt.FirstDispatchedAt, completed)-dwell.WorkingSeconds-dwell.GateWaitSeconds-dwell.MergeTrainSeconds-dwell.ParkedSeconds, 0)
	return dwell, unknown, rows.Err()
}
