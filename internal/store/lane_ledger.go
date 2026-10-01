package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

type LaneLedgerStore interface {
	LaneWriteLock() sync.Locker
	PrepareLaneWrite(context.Context, string, coordination.LaneWrite) (coordination.LaneWrite, error)
	ResolveLaneWrite(context.Context, uint64, string) error
	LatestLaneWrite(context.Context, IssueIdentity) (coordination.LaneWrite, string, error)
	LaneObservation(context.Context, IssueIdentity) (LaneObservation, error)
	SaveLaneObservation(context.Context, IssueIdentity, LaneObservation) error
}

func (s *sqliteStore) LaneWriteLock() sync.Locker {
	return &s.laneWriteMu
}

type LaneObservation struct {
	Origin    string
	State     string
	EnteredAt time.Time
}

func (s *sqliteStore) PrepareLaneWrite(ctx context.Context, project string, write coordination.LaneWrite) (coordination.LaneWrite, error) {
	if strings.TrimSpace(project) == "" || strings.TrimSpace(write.Issue) == "" || strings.TrimSpace(write.To) == "" || strings.TrimSpace(write.Reason) == "" {
		return coordination.LaneWrite{}, errors.New("lane write project, issue, target and reason are required")
	}
	at, err := requiredTimestamp("written_at", write.WrittenAt)
	if err != nil {
		return coordination.LaneWrite{}, err
	}
	identity, err := s.queries.LaneLedgerIdentity(ctx)
	if err != nil {
		return coordination.LaneWrite{}, fmt.Errorf("load lane ledger identity: %w", err)
	}
	row, err := s.queries.PrepareLaneWrite(ctx, sqlc.PrepareLaneWriteParams{ProjectID: project, IssueID: write.Issue, FromState: write.From, ToState: write.To, Reason: write.Reason, WrittenAt: at})
	if err != nil {
		return coordination.LaneWrite{}, fmt.Errorf("prepare lane write: %w", err)
	}
	if row.ID <= 0 {
		return coordination.LaneWrite{}, errors.New("lane write fence token is invalid")
	}
	write.InstanceIdentity = identity
	write.FenceToken = uint64(row.ID)
	return write, nil
}

func (s *sqliteStore) ResolveLaneWrite(ctx context.Context, token uint64, result string) error {
	if token == 0 || token > math.MaxInt64 || (result != "applied" && result != "failed" && result != "blocked" && result != "uncertain") {
		return errors.New("lane write token and valid result are required")
	}
	rows, err := s.queries.ResolveLaneWrite(ctx, sqlc.ResolveLaneWriteParams{ID: int64(token), Result: result, ResolvedAt: sql.NullString{String: time.Now().UTC().Format(time.RFC3339Nano), Valid: true}})
	if err != nil {
		return fmt.Errorf("resolve lane write: %w", err)
	}
	return requireAffected(rows, "lane write", int64(token))
}

func (s *sqliteStore) LatestLaneWrite(ctx context.Context, issue IssueIdentity) (coordination.LaneWrite, string, error) {
	row, err := s.queries.LatestLaneWrite(ctx, sqlc.LatestLaneWriteParams{ProjectID: issue.ProjectID, IssueID: issue.IssueID})
	if errors.Is(err, sql.ErrNoRows) {
		return coordination.LaneWrite{}, "", ErrNotFound
	}
	if err != nil {
		return coordination.LaneWrite{}, "", fmt.Errorf("load latest lane write: %w", err)
	}
	identity, err := s.queries.LaneLedgerIdentity(ctx)
	if err != nil {
		return coordination.LaneWrite{}, "", fmt.Errorf("load lane ledger identity: %w", err)
	}
	at, err := parseTimestamp("written_at", row.WrittenAt)
	if err != nil {
		return coordination.LaneWrite{}, "", err
	}
	if row.ID <= 0 {
		return coordination.LaneWrite{}, "", errors.New("lane write fence token is invalid")
	}
	return coordination.LaneWrite{InstanceIdentity: identity, Issue: row.IssueID, From: row.FromState, To: row.ToState, Reason: row.Reason, WrittenAt: at, FenceToken: uint64(row.ID)}, row.Result, nil
}

func (s *sqliteStore) LaneObservation(ctx context.Context, issue IssueIdentity) (LaneObservation, error) {
	row, err := s.queries.LaneObservation(ctx, sqlc.LaneObservationParams{ProjectID: issue.ProjectID, IssueID: issue.IssueID})
	if errors.Is(err, sql.ErrNoRows) {
		return LaneObservation{}, ErrNotFound
	}
	if err != nil {
		return LaneObservation{}, fmt.Errorf("load lane observation: %w", err)
	}
	at, err := parseTimestamp("entered_at", row.EnteredAt)
	if err != nil {
		return LaneObservation{}, err
	}
	return LaneObservation{State: row.State, EnteredAt: at, Origin: row.Origin}, nil
}

func (s *sqliteStore) SaveLaneObservation(ctx context.Context, issue IssueIdentity, observation LaneObservation) error {
	if strings.TrimSpace(issue.ProjectID) == "" || strings.TrimSpace(issue.IssueID) == "" || strings.TrimSpace(observation.State) == "" {
		return errors.New("lane observation project, issue and state are required")
	}
	at, err := requiredTimestamp("entered_at", observation.EnteredAt)
	if err != nil {
		return err
	}
	if err := s.queries.SaveLaneObservation(ctx, sqlc.SaveLaneObservationParams{ProjectID: issue.ProjectID, IssueID: issue.IssueID, State: observation.State, EnteredAt: at, Origin: observation.Origin}); err != nil {
		return fmt.Errorf("save lane observation: %w", err)
	}
	return nil
}

// RecordLaneWriteAction stamps a prepared lane write with the acting origin and
// action kind so the operations report can list it without a reason-code namespace.
func (s *sqliteStore) RecordLaneWriteAction(ctx context.Context, token uint64, origin, kind string) error {
	if token == 0 || token > math.MaxInt64 || strings.TrimSpace(origin) == "" {
		return errors.New("lane write token and origin are required")
	}
	rows, err := s.queries.RecordLaneWriteAction(ctx, sqlc.RecordLaneWriteActionParams{ID: int64(token), Origin: strings.TrimSpace(origin), ActionKind: strings.TrimSpace(kind)})
	if err != nil {
		return fmt.Errorf("record lane write action: %w", err)
	}
	return requireAffected(rows, "lane write", int64(token))
}

// ShippedOutcome is a verified delivery recorded by the existing lane writer.
// It does not imply that a worker session ended successfully.
type ShippedOutcome struct {
	IssueIdentity
	State       string
	CompletedAt time.Time
	PRNumber    int64
}

// ShippedOutcomeStore projects deliveries from the lane ledger without creating
// another completion owner or requiring a worker attempt.
type ShippedOutcomeStore interface {
	ShippedOutcomes(context.Context) ([]ShippedOutcome, error)
}

// The applied writer ledger records verified deliverables, unlike observed Done
// cards or successful sessions. Keep the first such outcome for an issue across
// all history so a repeated/imported observation cannot move it to another day.
func (s *sqliteStore) ShippedOutcomes(ctx context.Context) ([]ShippedOutcome, error) {
	rows, err := s.db.QueryContext(ctx, `
WITH outcomes AS (
 SELECT id, project_id, issue_id, to_state, written_at,
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
SELECT outcome.project_id, outcome.issue_id, outcome.to_state, outcome.written_at,
 COALESCE(phase.identifier, ''), COALESCE(phase.issue_url, ''), COALESCE(phase.pr_number, 0)
FROM outcomes AS outcome
LEFT JOIN workflow_phase_events AS phase ON phase.id = (
 SELECT id FROM workflow_phase_events
 WHERE project_id = outcome.project_id AND issue_id = outcome.issue_id
 ORDER BY id DESC LIMIT 1
)
WHERE outcome.ordinal = 1
ORDER BY outcome.project_id, outcome.issue_id`)
	if err != nil {
		return nil, fmt.Errorf("read shipped outcomes: %w", err)
	}
	defer rows.Close()
	var outcomes []ShippedOutcome
	for rows.Next() {
		var outcome ShippedOutcome
		var at string
		if err := rows.Scan(&outcome.ProjectID, &outcome.IssueID, &outcome.State, &at, &outcome.Identifier, &outcome.IssueURL, &outcome.PRNumber); err != nil {
			return nil, err
		}
		outcome.CompletedAt, err = parseStoredTime(at)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, rows.Err()
}
