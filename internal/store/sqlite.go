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

	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

type sqliteStore struct {
	db      *sql.DB
	queries *sqlc.Queries
	path    string
}

var _ Store = (*sqliteStore)(nil)

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
		path:    cfg.Path,
	}, nil
}

func (s *sqliteStore) Queries() *sqlc.Queries {
	return s.queries
}

func (s *sqliteStore) RuntimeEvidence(ctx context.Context, query RuntimeEvidenceQuery) (RuntimeEvidence, error) {
	evidence := RuntimeEvidence{
		Backend: BackendSQLite,
		Path:    s.path,
	}
	if err := s.db.PingContext(ctx); err != nil {
		return evidence, fmt.Errorf("pinging runtime store: %w", err)
	}
	evidence.Healthy = true

	version, err := s.runtimeMigrationVersion(ctx)
	if err != nil {
		return evidence, err
	}
	evidence.MigrationVersion = version
	evidence.MigrationStatus = fmt.Sprintf("applied through %d", version)

	tables := []struct {
		name          string
		projectScoped bool
	}{
		{name: "detent_runs"},
		{name: "codex_sessions"},
		{name: "fair_share_usage", projectScoped: true},
		{name: "usage_events", projectScoped: true},
		{name: "workflow_phase_events", projectScoped: true},
		{name: "work_attempts", projectScoped: true},
		{name: "scheduler_decisions", projectScoped: true},
	}
	projectID := strings.TrimSpace(query.ProjectID)
	for _, table := range tables {
		count, err := s.runtimeTableCount(ctx, table.name, table.projectScoped, projectID)
		if err != nil {
			return evidence, err
		}
		scope := "fleet"
		if table.projectScoped && projectID != "" {
			scope = "project"
		}
		evidence.Tables = append(evidence.Tables, RuntimeTableEvidence{
			Name:     table.name,
			Scope:    scope,
			RowCount: count,
		})
	}

	workflowEvidence, err := s.runtimeWorkflowPhaseEventEvidence(ctx, projectID)
	if err != nil {
		return evidence, err
	}
	evidence.WorkflowPhaseEvents = workflowEvidence

	return evidence, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func (s *sqliteStore) StartRun(ctx context.Context, attrs RunStart) (int64, error) {
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}

	run, err := s.queries.CreateDetentRun(ctx, sqlc.CreateDetentRunParams{
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
	rows, err := s.queries.UpdateDetentRun(ctx, sqlc.UpdateDetentRunParams{
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
	return requireAffected(rows, "detent run", runID)
}

func (s *sqliteStore) StopRun(ctx context.Context, runID int64, attrs RunStop) error {
	stoppedAt, err := requiredTimestamp("stopped_at", attrs.StoppedAt)
	if err != nil {
		return err
	}

	rows, err := s.queries.UpdateDetentRun(ctx, sqlc.UpdateDetentRunParams{
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
	return requireAffected(rows, "detent run", runID)
}

func (s *sqliteStore) StartSession(ctx context.Context, attrs SessionStart) (int64, error) {
	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}

	session, err := s.queries.CreateCodexSession(ctx, sqlc.CreateCodexSessionParams{
		RunID:       nullPositiveInt64(attrs.RunID),
		IssueID:     nullString(attrs.IssueID),
		Identifier:  nullString(attrs.Identifier),
		IssueURL:    nullString(attrs.IssueURL),
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

func (s *sqliteStore) RecordUsageEvent(ctx context.Context, attrs UsageEvent) (int64, error) {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return 0, errors.New("project_id is required")
	}

	startedAt, err := requiredTimestamp("started_at", attrs.StartedAt)
	if err != nil {
		return 0, err
	}
	finishedAt, err := requiredTimestamp("finished_at", attrs.FinishedAt)
	if err != nil {
		return 0, err
	}

	outcome := strings.TrimSpace(attrs.Outcome)
	if outcome == "" {
		return 0, errors.New("outcome is required")
	}

	event, err := s.queries.CreateUsageEvent(ctx, sqlc.CreateUsageEventParams{
		ProjectID:      projectID,
		RunID:          nullPositiveInt64(attrs.RunID),
		SessionID:      nullPositiveInt64(attrs.SessionID),
		IssueID:        nullString(attrs.IssueID),
		Identifier:     nullString(attrs.Identifier),
		PrNumber:       nullOptionalInt64(attrs.PRNumber),
		Model:          strings.TrimSpace(attrs.Model),
		InputTokens:    nonNegative(attrs.InputTokens),
		OutputTokens:   nonNegative(attrs.OutputTokens),
		TotalTokens:    nonNegative(attrs.TotalTokens),
		CostUsd:        nonNegativeFloat(attrs.CostUSD),
		RuntimeSeconds: nonNegative(attrs.RuntimeSeconds),
		StartedAt:      startedAt,
		FinishedAt:     finishedAt,
		EventDay:       attrs.FinishedAt.UTC().Format("2006-01-02"),
		Outcome:        outcome,
	})
	if err != nil {
		return 0, fmt.Errorf("recording usage event: %w", err)
	}
	return event.ID, nil
}

func (s *sqliteStore) UsageReport(ctx context.Context, query UsageReportQuery) (UsageReport, error) {
	group, err := normalizeUsageReportGroup(query.By)
	if err != nil {
		return UsageReport{}, err
	}
	from, err := optionalDateString(query.From)
	if err != nil {
		return UsageReport{}, err
	}
	to, err := optionalDateString(query.To)
	if err != nil {
		return UsageReport{}, err
	}
	if from != "" && to != "" && from > to {
		return UsageReport{}, errors.New("from date must be on or before to date")
	}

	rows, err := s.queries.UsageReportRows(ctx, sqlc.UsageReportRowsParams{
		BucketBy: string(group),
		FromDay:  nullString(from),
		ToDay:    nullString(to),
	})
	if err != nil {
		return UsageReport{}, fmt.Errorf("reading usage report: %w", err)
	}

	report := UsageReport{
		By:   group,
		From: from,
		To:   to,
		Rows: []UsageReportRow{},
		Totals: UsageReportTotals{
			Models: []UsageReportModel{},
		},
	}
	rowByKey := map[string]int{}
	modelTotals := map[string]int{}
	for _, row := range rows {
		key := row.GroupKey
		if key == "" {
			key = "unassigned"
		}

		index, ok := rowByKey[key]
		if !ok {
			report.Rows = append(report.Rows, UsageReportRow{
				Key:    key,
				Models: []UsageReportModel{},
			})
			index = len(report.Rows) - 1
			rowByKey[key] = index
		}

		model := UsageReportModel{
			Model:          row.Model,
			InputTokens:    row.InputTokens,
			OutputTokens:   row.OutputTokens,
			TotalTokens:    row.TotalTokens,
			RuntimeSeconds: row.RuntimeSeconds,
			Events:         row.Events,
		}
		if model.Model == "" {
			model.Model = "unassigned"
		}

		report.Rows[index].InputTokens += model.InputTokens
		report.Rows[index].OutputTokens += model.OutputTokens
		report.Rows[index].TotalTokens += model.TotalTokens
		report.Rows[index].RuntimeSeconds += model.RuntimeSeconds
		report.Rows[index].Events += model.Events
		report.Rows[index].Models = append(report.Rows[index].Models, model)

		report.Totals.InputTokens += model.InputTokens
		report.Totals.OutputTokens += model.OutputTokens
		report.Totals.TotalTokens += model.TotalTokens
		report.Totals.RuntimeSeconds += model.RuntimeSeconds
		report.Totals.Events += model.Events

		modelIndex, ok := modelTotals[model.Model]
		if !ok {
			report.Totals.Models = append(report.Totals.Models, UsageReportModel{
				Model: model.Model,
			})
			modelIndex = len(report.Totals.Models) - 1
			modelTotals[model.Model] = modelIndex
		}
		report.Totals.Models[modelIndex].InputTokens += model.InputTokens
		report.Totals.Models[modelIndex].OutputTokens += model.OutputTokens
		report.Totals.Models[modelIndex].TotalTokens += model.TotalTokens
		report.Totals.Models[modelIndex].RuntimeSeconds += model.RuntimeSeconds
		report.Totals.Models[modelIndex].Events += model.Events
	}

	return report, nil
}

func (s *sqliteStore) BudgetCostEvents(ctx context.Context, query BudgetCostQuery) ([]BudgetCostEvent, error) {
	from, err := requiredTimestamp("from", query.From)
	if err != nil {
		return nil, err
	}
	to, err := requiredTimestamp("to", query.To)
	if err != nil {
		return nil, err
	}
	if !query.To.After(query.From) {
		return nil, errors.New("from must be before to")
	}

	rows, err := s.queries.BudgetCostEvents(ctx, sqlc.BudgetCostEventsParams{
		FromTime: from,
		ToTime:   to,
	})
	if err != nil {
		return nil, fmt.Errorf("reading budget cost events: %w", err)
	}

	projectFilter := budgetCostProjectFilter(query.ProjectIDs)
	events := make([]BudgetCostEvent, 0, len(rows))
	for _, row := range rows {
		projectID := strings.TrimSpace(row.ProjectID)
		if !projectFilter(projectID) {
			continue
		}
		at, err := parseTimestamp("finished_at", row.FinishedAt)
		if err != nil {
			return nil, err
		}
		events = append(events, BudgetCostEvent{
			ProjectID: projectID,
			At:        at.UTC(),
			CostUSD:   nonNegativeFloat(row.CostUsd),
		})
	}
	return events, nil
}

func (s *sqliteStore) LifetimeTotals(ctx context.Context) (LifetimeTotals, error) {
	row, err := s.queries.LifetimeTotals(ctx)
	if err != nil {
		return LifetimeTotals{}, fmt.Errorf("reading lifetime totals: %w", err)
	}
	return LifetimeTotals{
		InputTokens:    row.InputTokens,
		OutputTokens:   row.OutputTokens,
		TotalTokens:    row.TotalTokens,
		RuntimeSeconds: row.RuntimeSeconds,
		Sessions:       row.Sessions,
		Runs:           row.Runs,
	}, nil
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

func (s *sqliteStore) IssueTokenSpend(ctx context.Context, identity IssueIdentity) (TokenSpend, error) {
	identity = normalizeIssueIdentity(identity)
	if identity.IssueID == "" && identity.Identifier == "" && identity.IssueURL == "" {
		return TokenSpend{ByModel: []ModelTokenSpend{}}, nil
	}

	rows, err := s.queries.IssueTokenSpend(ctx, sqlc.IssueTokenSpendParams{
		IssueID:    nullString(identity.IssueID),
		Identifier: nullString(identity.Identifier),
		IssueURL:   nullString(identity.IssueURL),
	})
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

func (s *sqliteStore) CycleTimeReport(ctx context.Context) (CycleTimeReport, error) {
	rows, err := s.queries.CompletedIssueCycleRows(ctx)
	if err != nil {
		return CycleTimeReport{}, fmt.Errorf("reading cycle time report: %w", err)
	}

	issues := make([]CycleTimeIssue, 0, len(rows))
	for _, row := range rows {
		startedAt, err := parseTimestamp("started_at", row.StartedAt)
		if err != nil {
			return CycleTimeReport{}, err
		}
		completedAt, err := parseTimestamp("completed_at", row.CompletedAt)
		if err != nil {
			return CycleTimeReport{}, err
		}
		seconds, ok := cycleTimeSeconds(startedAt, completedAt)
		if !ok {
			continue
		}
		issues = append(issues, CycleTimeIssue{
			Key:             row.IssueKey,
			StartedAt:       startedAt,
			CompletedAt:     completedAt,
			DurationSeconds: seconds,
			Sessions:        row.Sessions,
		})
	}

	return CycleTimeReport{
		Issues:         issues,
		Buckets:        cycleTimeBuckets(issues),
		AverageSeconds: averageCycleTimeSeconds(issues),
	}, nil
}

func (s *sqliteStore) ListFairShareUsage(ctx context.Context) ([]FairShareUsage, error) {
	rows, err := s.queries.ListFairShareUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading fair-share usage: %w", err)
	}

	usage := make([]FairShareUsage, 0, len(rows))
	for _, row := range rows {
		updatedAt, err := parseTimestamp("updated_at", row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		usage = append(usage, FairShareUsage{
			ProjectID:      row.ProjectID,
			Weight:         int(row.Weight),
			Dispatches:     row.Dispatches,
			RuntimeSeconds: row.RuntimeSeconds,
			UpdatedAt:      updatedAt,
		})
	}
	return usage, nil
}

func (s *sqliteStore) RecordFairShareDispatch(ctx context.Context, attrs FairShareDispatch) error {
	projectID := strings.TrimSpace(attrs.ProjectID)
	if projectID == "" {
		return errors.New("project_id is required")
	}

	dispatchedAt, err := requiredTimestamp("dispatched_at", attrs.DispatchedAt)
	if err != nil {
		return err
	}

	_, err = s.queries.UpsertFairShareUsage(ctx, sqlc.UpsertFairShareUsageParams{
		ProjectID:      projectID,
		Weight:         int64(positiveWeight(attrs.Weight)),
		RuntimeSeconds: nonNegative(attrs.RuntimeSeconds),
		UpdatedAt:      dispatchedAt,
	})
	if err != nil {
		return fmt.Errorf("recording fair-share dispatch: %w", err)
	}
	return nil
}

func normalizeIssueIdentity(identity IssueIdentity) IssueIdentity {
	return IssueIdentity{
		IssueID:    strings.TrimSpace(identity.IssueID),
		Identifier: strings.TrimSpace(identity.Identifier),
		IssueURL:   strings.TrimSpace(identity.IssueURL),
	}
}

func budgetCostProjectFilter(projectIDs []string) func(string) bool {
	allowed := make(map[string]struct{}, len(projectIDs))
	for _, projectID := range projectIDs {
		projectID = strings.TrimSpace(projectID)
		if projectID == "" {
			continue
		}
		allowed[projectID] = struct{}{}
	}
	if len(allowed) == 0 {
		return func(string) bool {
			return true
		}
	}
	return func(projectID string) bool {
		_, ok := allowed[strings.TrimSpace(projectID)]
		return ok
	}
}

func normalizeUsageReportGroup(group UsageReportGroup) (UsageReportGroup, error) {
	switch group {
	case "", UsageReportByDay:
		return UsageReportByDay, nil
	case UsageReportByProject, UsageReportByIssue, UsageReportByPR, UsageReportByModel:
		return group, nil
	default:
		return "", fmt.Errorf("unsupported usage report group %q", group)
	}
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

func (s *sqliteStore) runtimeMigrationVersion(ctx context.Context) (int64, error) {
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied = 1").Scan(&version); err != nil {
		return 0, fmt.Errorf("reading migration status: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

func (s *sqliteStore) runtimeTableCount(ctx context.Context, tableName string, projectScoped bool, projectID string) (int64, error) {
	var row *sql.Row
	if projectScoped && projectID != "" {
		switch tableName {
		case "fair_share_usage":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM fair_share_usage WHERE project_id = ?", projectID)
		case "usage_events":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_events WHERE project_id = ?", projectID)
		case "workflow_phase_events":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_phase_events WHERE project_id = ?", projectID)
		case "work_attempts":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM work_attempts WHERE project_id = ?", projectID)
		case "scheduler_decisions":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_decisions WHERE project_id = ?", projectID)
		default:
			return 0, fmt.Errorf("unsupported project-scoped runtime table %q", tableName)
		}
	} else {
		switch tableName {
		case "detent_runs":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM detent_runs")
		case "codex_sessions":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM codex_sessions")
		case "fair_share_usage":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM fair_share_usage")
		case "usage_events":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_events")
		case "workflow_phase_events":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_phase_events")
		case "work_attempts":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM work_attempts")
		case "scheduler_decisions":
			row = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_decisions")
		default:
			return 0, fmt.Errorf("unsupported runtime table %q", tableName)
		}
	}

	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("counting runtime table %s: %w", tableName, err)
	}
	return count, nil
}

func (s *sqliteStore) runtimeWorkflowPhaseEventEvidence(ctx context.Context, projectID string) (RuntimeWorkflowPhaseEventEvidence, error) {
	var row *sql.Row
	if projectID != "" {
		row = s.db.QueryRowContext(ctx, "SELECT COUNT(*), MIN(finished_at), MAX(finished_at) FROM workflow_phase_events WHERE project_id = ?", projectID)
	} else {
		row = s.db.QueryRowContext(ctx, "SELECT COUNT(*), MIN(finished_at), MAX(finished_at) FROM workflow_phase_events")
	}

	var count int64
	var oldest sql.NullString
	var newest sql.NullString
	if err := row.Scan(&count, &oldest, &newest); err != nil {
		return RuntimeWorkflowPhaseEventEvidence{}, fmt.Errorf("reading workflow phase event evidence: %w", err)
	}
	return RuntimeWorkflowPhaseEventEvidence{
		RowCount:         count,
		OldestFinishedAt: nullableRuntimeTimestamp(oldest),
		NewestFinishedAt: nullableRuntimeTimestamp(newest),
	}, nil
}

func nullableRuntimeTimestamp(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value.String))
	if err != nil {
		return nil
	}
	return &parsed
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

func optionalDateString(value time.Time) (string, error) {
	if value.IsZero() {
		return "", nil
	}
	return dateString(value)
}

func parseTimestamp(name string, value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, fmt.Errorf("%s is required", name)
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func nullString(value string) sql.NullString {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: trimmed, Valid: true}
}

func nullPositiveInt64(value int64) sql.NullInt64 {
	if value <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: value, Valid: true}
}

func nullOptionalInt64(value *int64) sql.NullInt64 {
	if value == nil || *value <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func nonNegativeFloat(value float64) float64 {
	if value < 0 {
		return 0
	}
	return value
}

func positiveWeight(value int) int {
	if value <= 0 {
		return 1
	}
	return value
}

func requireAffected(rows int64, name string, id int64) error {
	if rows == 0 {
		return fmt.Errorf("%w: %s %d", ErrNotFound, name, id)
	}
	return nil
}
