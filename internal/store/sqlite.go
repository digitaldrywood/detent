package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/symphony-go/internal/store/sqlc"
)

type sqliteStore struct {
	db      *sql.DB
	queries *sqlc.Queries
}

func openSQLite(ctx context.Context, cfg Config) (*sqliteStore, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqlite path is required")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("creating sqlite directory: %w", err)
	}

	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := configureSQLite(ctx, db, busyTimeoutMillis(cfg.BusyTimeout)); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := runMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &sqliteStore{
		db:      db,
		queries: sqlc.New(db),
	}, nil
}

func (s *sqliteStore) Queries() *sqlc.Queries {
	return s.queries
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func (s *sqliteStore) StartRun(ctx context.Context, attrs RunStart) (int64, error) {
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}

	run, err := s.queries.CreateSymphonyRun(ctx, sqlc.CreateSymphonyRunParams{
		StartedAt:            startedAt,
		StoppedAt:            sql.NullString{},
		RestartReason:        sql.NullString{},
		PeakConcurrentAgents: nonNegative(attrs.PeakConcurrentAgents),
		SessionsLaunched:     nonNegative(attrs.SessionsLaunched),
		InputTokens:          nonNegative(attrs.InputTokens),
		OutputTokens:         nonNegative(attrs.OutputTokens),
		TotalTokens:          nonNegative(attrs.TotalTokens),
		RuntimeSeconds:       nonNegative(attrs.RuntimeSeconds),
	})
	if err != nil {
		return 0, fmt.Errorf("starting stats run: %w", err)
	}
	return run.ID, nil
}

func (s *sqliteStore) UpdateRun(ctx context.Context, runID int64, attrs RunUpdate) error {
	rows, err := s.queries.UpdateSymphonyRun(ctx, sqlc.UpdateSymphonyRunParams{
		StoppedAt:            sql.NullString{},
		RestartReason:        sql.NullString{},
		PeakConcurrentAgents: nonNegative(attrs.PeakConcurrentAgents),
		SessionsLaunched:     nonNegative(attrs.SessionsLaunched),
		InputTokens:          nonNegative(attrs.InputTokens),
		OutputTokens:         nonNegative(attrs.OutputTokens),
		TotalTokens:          nonNegative(attrs.TotalTokens),
		RuntimeSeconds:       nonNegative(attrs.RuntimeSeconds),
		ID:                   runID,
	})
	if err != nil {
		return fmt.Errorf("updating stats run: %w", err)
	}
	return requireAffected(rows, "symphony run", runID)
}

func (s *sqliteStore) StopRun(ctx context.Context, runID int64, attrs RunStop) error {
	stoppedAt, err := requiredTimestamp("stopped_at", attrs.StoppedAt)
	if err != nil {
		return err
	}

	rows, err := s.queries.UpdateSymphonyRun(ctx, sqlc.UpdateSymphonyRunParams{
		StoppedAt:            sql.NullString{String: stoppedAt, Valid: true},
		RestartReason:        nullString(attrs.RestartReason),
		PeakConcurrentAgents: nonNegative(attrs.PeakConcurrentAgents),
		SessionsLaunched:     nonNegative(attrs.SessionsLaunched),
		InputTokens:          nonNegative(attrs.InputTokens),
		OutputTokens:         nonNegative(attrs.OutputTokens),
		TotalTokens:          nonNegative(attrs.TotalTokens),
		RuntimeSeconds:       nonNegative(attrs.RuntimeSeconds),
		ID:                   runID,
	})
	if err != nil {
		return fmt.Errorf("stopping stats run: %w", err)
	}
	return requireAffected(rows, "symphony run", runID)
}

func (s *sqliteStore) StartSession(ctx context.Context, attrs SessionStart) (int64, error) {
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}

	session, err := s.queries.CreateCodexSession(ctx, sqlc.CreateCodexSessionParams{
		RunID:       nullInt64(attrs.RunID),
		IssueID:     nullString(attrs.IssueID),
		Identifier:  nullString(attrs.Identifier),
		IssueUrl:    nullString(attrs.IssueURL),
		StartedAt:   sql.NullString{String: startedAt, Valid: true},
		CompletedAt: sql.NullString{},
		FinalState:  sql.NullString{},
		Model:       nullString(attrs.Model),
	})
	if err != nil {
		return 0, fmt.Errorf("starting codex session: %w", err)
	}
	return session.ID, nil
}

func (s *sqliteStore) FinishSession(ctx context.Context, sessionID int64, attrs SessionFinish) error {
	completedAt, err := requiredTimestamp("completed_at", attrs.CompletedAt)
	if err != nil {
		return err
	}

	rows, err := s.queries.FinishCodexSession(ctx, sqlc.FinishCodexSessionParams{
		CompletedAt:    sql.NullString{String: completedAt, Valid: true},
		Turns:          nonNegative(attrs.Turns),
		InputTokens:    nonNegative(attrs.InputTokens),
		OutputTokens:   nonNegative(attrs.OutputTokens),
		TotalTokens:    nonNegative(attrs.TotalTokens),
		RuntimeSeconds: nonNegative(attrs.RuntimeSeconds),
		FinalState:     nullString(attrs.FinalState),
		Model:          nullString(attrs.Model),
		ID:             sessionID,
	})
	if err != nil {
		return fmt.Errorf("finishing codex session: %w", err)
	}
	return requireAffected(rows, "codex session", sessionID)
}

func (s *sqliteStore) DailyTokenSpend(ctx context.Context, day time.Time) (TokenSpend, error) {
	date, err := dateString(day)
	if err != nil {
		return TokenSpend{}, err
	}

	rows, err := s.queries.DailyTokenSpend(ctx, sql.NullString{String: date, Valid: true})
	if err != nil {
		return TokenSpend{}, fmt.Errorf("reading daily token spend: %w", err)
	}

	spend := TokenSpend{
		Date:    date,
		ByModel: make([]ModelTokenSpend, 0, len(rows)),
	}
	for _, row := range rows {
		modelSpend := ModelTokenSpend{
			Model:        row.Model,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			TotalTokens:  row.TotalTokens,
			Sessions:     row.Sessions,
		}
		spend.InputTokens += modelSpend.InputTokens
		spend.OutputTokens += modelSpend.OutputTokens
		spend.TotalTokens += modelSpend.TotalTokens
		spend.Sessions += modelSpend.Sessions
		spend.ByModel = append(spend.ByModel, modelSpend)
	}
	return spend, nil
}

func (s *sqliteStore) IssueTokenSpend(ctx context.Context, issueID string) (TokenSpend, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return TokenSpend{ByModel: []ModelTokenSpend{}}, nil
	}

	rows, err := s.queries.IssueTokenSpend(ctx, sql.NullString{String: issueID, Valid: true})
	if err != nil {
		return TokenSpend{}, fmt.Errorf("reading issue token spend: %w", err)
	}

	spend := TokenSpend{
		ByModel: make([]ModelTokenSpend, 0, len(rows)),
	}
	for _, row := range rows {
		modelSpend := ModelTokenSpend{
			Model:        row.Model,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			TotalTokens:  row.TotalTokens,
			Sessions:     row.Sessions,
		}
		spend.InputTokens += modelSpend.InputTokens
		spend.OutputTokens += modelSpend.OutputTokens
		spend.TotalTokens += modelSpend.TotalTokens
		spend.Sessions += modelSpend.Sessions
		spend.ByModel = append(spend.ByModel, modelSpend)
	}
	return spend, nil
}

func configureSQLite(ctx context.Context, db *sql.DB, busyTimeoutMillis int64) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMillis)); err != nil {
		return fmt.Errorf("setting sqlite busy_timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enabling sqlite foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enabling sqlite WAL: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging sqlite database: %w", err)
	}
	return nil
}

func requiredTimestamp(name string, value time.Time) (string, error) {
	if value.IsZero() {
		return "", fmt.Errorf("%s is required", name)
	}
	return value.UTC().Truncate(time.Second).Format(time.RFC3339), nil
}

func dateString(value time.Time) (string, error) {
	if value.IsZero() {
		return "", errors.New("date is required")
	}
	return value.Format("2006-01-02"), nil
}

func nullString(value string) sql.NullString {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: trimmed, Valid: true}
}

func nullInt64(value int64) sql.NullInt64 {
	if value <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func requireAffected(rows int64, name string, id int64) error {
	if rows == 0 {
		return fmt.Errorf("%w: %s %d", ErrNotFound, name, id)
	}
	return nil
}
