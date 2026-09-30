package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/efficiency"
)

type dailyShippedOutcome struct {
	project, issue string
	at             time.Time
}

// The applied writer ledger records verified deliverables, unlike observed Done
// cards or successful sessions. Keep the first such outcome for an issue across
// all history so a repeated/imported observation cannot move it to another day.
func (s *sqliteStore) dailyShippedOutcomes(ctx context.Context) ([]dailyShippedOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH outcomes AS (
 SELECT project_id, issue_id, written_at,
   ROW_NUMBER() OVER (PARTITION BY project_id, issue_id ORDER BY julianday(written_at), id) AS ordinal
 FROM lane_ledger AS lane
 WHERE result = 'applied' AND (
   reason IN ('merge_worker_programmatic_merge', 'pull_request_merged',
     'closed_completed_running_done', 'issue_closed_completed', 'operational_completion')
   OR (reason = 'ready' AND EXISTS (
     SELECT 1 FROM workflow_phase_events AS phase
     WHERE phase.project_id = lane.project_id AND phase.issue_id = lane.issue_id
       AND phase.phase_type = 'lane' AND phase.status = 'entered'
       AND phase.phase_name = lane.to_state AND phase.started_at = lane.written_at
       AND json_extract(phase.metadata_json, '$.terminal_outcome') = 'artifact'
   ))
 )
)
SELECT project_id, issue_id, written_at FROM outcomes WHERE ordinal = 1`)
	if err != nil {
		return nil, fmt.Errorf("read daily shipped outcomes: %w", err)
	}
	defer rows.Close()
	var outcomes []dailyShippedOutcome
	for rows.Next() {
		var outcome dailyShippedOutcome
		var at string
		if err := rows.Scan(&outcome.project, &outcome.issue, &at); err != nil {
			return nil, err
		}
		outcome.at, err = parseStoredTime(at)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, rows.Err()
}

func (s *sqliteStore) populateDailyShippedCohort(ctx context.Context, day *DailyDigestDay, window DailyDigestWindow, outcomes []dailyShippedOutcome) error {
	day.ShippedByProject = map[string]int64{}
	var receipts []efficiency.Receipt
	for _, outcome := range outcomes {
		if outcome.at.Before(window.From) || !outcome.at.Before(window.To) {
			continue
		}
		day.IssuesShipped++
		day.ShippedByProject[outcome.project]++
		receipt, err := s.EfficiencyReceipt(ctx, outcome.project, outcome.issue, "")
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
		dwell, unknown, err := s.dailyCohortDwell(ctx, receipt, outcome.at)
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
 FROM workflow_phase_events
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
