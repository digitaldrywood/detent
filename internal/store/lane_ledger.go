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
	rows, err := s.queries.ResolveLaneWrite(ctx, sqlc.ResolveLaneWriteParams{ID: int64(token), Result: result})
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
