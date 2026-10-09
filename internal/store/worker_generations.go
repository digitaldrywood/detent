package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

const (
	WorkerGenerationStateActive    = "active"
	WorkerGenerationStateExited    = "exited"
	WorkerGenerationStateAbandoned = "abandoned"
)

const emptyGenerations = "[]"

type WorkerGenerationStore interface {
	LiveForeignGenerationReader
	StartWorkerGeneration(context.Context, WorkerGenerationStart) (int64, error)
	ExitWorkerGeneration(context.Context, time.Time) error
	WorkerGeneration() int64
	LiveWorkerGenerations(context.Context) ([]int64, error)
}

type LiveForeignGenerationReader interface {
	LiveForeignWorkerGenerations(context.Context) ([]int64, error)
}

type WorkerGenerationStart struct {
	PID          int
	ProcessStart string
	Version      string
	StartedAt    time.Time
}

func (s *sqliteStore) StartWorkerGeneration(ctx context.Context, attrs WorkerGenerationStart) (int64, error) {
	if attrs.PID <= 0 {
		return 0, errors.New("worker generation pid is required")
	}
	processStart := strings.TrimSpace(attrs.ProcessStart)
	if processStart == "" {
		return 0, errors.New("worker generation process start is required")
	}
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}
	unexited, err := s.queries.ListUnexitedWorkerGenerations(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing worker generations: %w", err)
	}
	dead := make([]int64, 0, len(unexited))
	for _, row := range unexited {
		if !workerGenerationLive(row) {
			dead = append(dead, row.Gen)
		}
	}
	deadJSON, err := json.Marshal(dead)
	if err != nil {
		return 0, fmt.Errorf("encoding dead worker generations: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("beginning worker generation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if _, err := queries.ExitWorkerGenerations(ctx, sqlc.ExitWorkerGenerationsParams{
		State:       WorkerGenerationStateAbandoned,
		ExitedAt:    sql.NullString{String: startedAt, Valid: true},
		Generations: string(deadJSON),
	}); err != nil {
		return 0, fmt.Errorf("abandoning dead worker generations: %w", err)
	}
	generation, err := queries.CreateWorkerGeneration(ctx, sqlc.CreateWorkerGenerationParams{
		Pid:          int64(attrs.PID),
		ProcessStart: processStart,
		Version:      strings.TrimSpace(attrs.Version),
		State:        WorkerGenerationStateActive,
		StartedAt:    startedAt,
		UpdatedAt:    startedAt,
	})
	if err != nil {
		return 0, fmt.Errorf("allocating worker generation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing worker generation: %w", err)
	}
	s.generation.Store(generation)
	return generation, nil
}

func (s *sqliteStore) ExitWorkerGeneration(ctx context.Context, exitedAt time.Time) error {
	generation := s.generation.Load()
	if generation <= 0 {
		return nil
	}
	timestamp, err := requiredTimestamp("exited_at", exitedAt)
	if err != nil {
		return err
	}
	if _, err := s.queries.ExitWorkerGenerations(ctx, sqlc.ExitWorkerGenerationsParams{
		State:       WorkerGenerationStateExited,
		ExitedAt:    sql.NullString{String: timestamp, Valid: true},
		Generations: fmt.Sprintf("[%d]", generation),
	}); err != nil {
		return fmt.Errorf("marking worker generation %d exited: %w", generation, err)
	}
	return nil
}

func (s *sqliteStore) WorkerGeneration() int64 {
	return s.generation.Load()
}

func (s *sqliteStore) LiveWorkerGenerations(ctx context.Context) ([]int64, error) {
	rows, err := s.queries.ListUnexitedWorkerGenerations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing worker generations: %w", err)
	}
	current := s.generation.Load()
	live := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.Gen == current || workerGenerationLive(row) {
			live = append(live, row.Gen)
		}
	}
	return live, nil
}

func (s *sqliteStore) LiveForeignWorkerGenerations(ctx context.Context) ([]int64, error) {
	live, err := s.LiveWorkerGenerations(ctx)
	if err != nil {
		return nil, err
	}
	current := s.generation.Load()
	return slices.DeleteFunc(live, func(generation int64) bool { return generation == current }), nil
}

func (s *sqliteStore) liveForeignGenerationsJSON(ctx context.Context) (string, error) {
	foreign, err := s.LiveForeignWorkerGenerations(ctx)
	if err != nil {
		return "", err
	}
	if len(foreign) == 0 {
		return emptyGenerations, nil
	}
	encoded, err := json.Marshal(foreign)
	if err != nil {
		return "", fmt.Errorf("encoding live worker generations: %w", err)
	}
	return string(encoded), nil
}

func (s *sqliteStore) ownerGeneration() sql.NullInt64 {
	return nullPositiveInt64(s.generation.Load())
}

func workerGenerationLive(row sqlc.ListUnexitedWorkerGenerationsRow) bool {
	live, err := procstart.Matches(int(row.Pid), row.ProcessStart)
	if err != nil {
		return true
	}
	return live
}
