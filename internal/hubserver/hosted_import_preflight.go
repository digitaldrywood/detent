package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const hostedImportFallbackRows = 1000

type hostedImportFieldsKey struct{}

type hostedImportFieldSnapshot map[string]map[string]hostedImportFieldValues

func loadHostedImportFields(ctx context.Context, reader nativeQueryer, table, selection string, args ...any) (map[string]hostedImportFieldValues, error) {
	var projection hostedImportProjection
	for _, candidate := range hostedImportProjections {
		if candidate.table == table {
			projection = candidate
			break
		}
	}
	if projection.table == "" {
		return nil, errors.New("unknown hosted import projection")
	}
	unknown := make([]string, len(projection.columns))
	for i, column := range projection.columns {
		unknown[i] = column + " IS NULL"
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	rows, err := reader.QueryContext(readCtx, `SELECT COALESCE(`+projection.payload+`,'null') FROM `+table+` WHERE (`+strings.Join(unknown, " OR ")+`) AND (`+selection+`) LIMIT ?`, append(args, hostedImportFallbackRows+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]hostedImportFieldValues)
	count := 0
	for rows.Next() {
		count++
		if count > hostedImportFallbackRows {
			return nil, errors.New("legacy typed fields await bounded maintenance")
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		fields, err := hostedImportFields(table, raw)
		if err != nil {
			return nil, fmt.Errorf("evaluate %s legacy fields: %w", table, err)
		}
		values[raw] = fields
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func prepareHostedImportFields(ctx context.Context, reader nativeQueryer, table, selection string, args ...any) (context.Context, error) {
	values, err := loadHostedImportFields(ctx, reader, table, selection, args...)
	if err != nil {
		return ctx, err
	}
	snapshot := make(hostedImportFieldSnapshot)
	if previous, ok := ctx.Value(hostedImportFieldsKey{}).(hostedImportFieldSnapshot); ok {
		for key, value := range previous {
			snapshot[key] = value
		}
	}
	snapshot[table] = values
	return context.WithValue(ctx, hostedImportFieldsKey{}, snapshot), nil
}

func evaluatedHostedImportFields(ctx context.Context, query nativeQueryer, table, raw string) (hostedImportFieldValues, error) {
	if snapshot, ok := ctx.Value(hostedImportFieldsKey{}).(hostedImportFieldSnapshot); ok {
		if values, found := snapshot[table][raw]; found {
			return values, nil
		}
	}
	if _, writing := query.(*sql.Tx); writing {
		return hostedImportFieldValues{}, errors.New("legacy typed field evaluation requires a current reader snapshot")
	}
	return hostedImportFields(table, raw)
}

func (s *Service) prepareHostedImportMutation(ctx context.Context, scope nativeScope, operation string, input any) (context.Context, error) {
	snapshot := make(hostedImportFieldSnapshot)
	var tables []string
	switch input.(type) {
	case tracker.GitHubBatchCommand, tracker.GitHubBatchResult:
		tables = []string{"issues", "github_import_records", "runner_identities"}
	case CutoverRequest:
		tables = []string{"github_import_records", "github_outbox"}
	case updateProjectIntegrationRequest:
		tables = []string{"github_outbox"}
	case spriteUsageRequest:
		tables = []string{"usage_cost_observations"}
	}
	if strings.Contains(operation, "/imports/") {
		tables = append(tables, "github_import_records")
	}
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Data.AttemptID != "" {
		values, err := loadHostedImportFields(ctx, s.database.reader, "usage_cost_observations", `organization_id=? AND project_id=? AND resource_id=?`, scope.organization, scope.project, event.Data.AttemptID)
		if err != nil {
			return ctx, err
		}
		snapshot["usage_cost_observations"] = values
	}
	for _, table := range tables {
		selection := "project_id=?"
		args := []any{scope.project}
		switch table {
		case "issues", "usage_cost_observations":
			selection = "organization_id=? AND project_id=?"
			args = []any{scope.organization, scope.project}
		case "github_import_records":
			selection = "kind='dependency' AND import_id IN (SELECT id FROM github_imports WHERE project_id=?)"
		case "github_outbox":
			selection = "status IN ('pending','retrying') AND mutation_kind='workpad' AND issue_id IN (SELECT id FROM issues WHERE project_id=?)"
		case "runner_identities":
			selection = "organization_id=? AND id IN (SELECT runner_id FROM runner_checkout_repositories WHERE project_id=?)"
			args = []any{scope.organization, scope.project}
		}
		values, err := loadHostedImportFields(ctx, s.database.reader, table, selection, args...)
		if err != nil {
			return ctx, err
		}
		snapshot[table] = values
	}
	return context.WithValue(ctx, hostedImportFieldsKey{}, snapshot), nil
}

func (s *Service) prepareOutboxClaimFields(ctx context.Context) (context.Context, error) {
	now := s.config.now()
	var raw string
	err := s.database.reader.QueryRowContext(ctx, `SELECT o.desired_json FROM github_outbox o JOIN issues i ON i.id=o.issue_id JOIN projects p ON p.id=i.project_id JOIN repositories r ON r.id=o.repository_id WHERE o.desired_issue_number IS NULL AND ((o.status IN (?,?) AND (o.next_retry_at IS NULL OR o.next_retry_at<=?)) OR (o.status=? AND o.processing_started_at<=?)) ORDER BY o.id LIMIT 1`, outboxPending, outboxRetrying, formatOutboxTime(now), outboxProcessing, formatOutboxTime(now.Add(-s.config.OutboxProcessingTimeout))).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ctx, nil
	}
	if err != nil {
		return ctx, err
	}
	fields, err := hostedImportFields("github_outbox", raw)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, hostedImportFieldsKey{}, hostedImportFieldSnapshot{"github_outbox": {raw: fields}}), nil
}

func cachedRunnerOperations(ctx context.Context, query nativeQueryer, raw string) ([]string, error) {
	fields, err := evaluatedHostedImportFields(ctx, query, "runner_identities", raw)
	if err != nil {
		return nil, err
	}
	return fields.operations, nil
}

func decodeRunnerFields(raw string) (hostedImportFieldValues, error) {
	var operations []string
	if err := json.Unmarshal([]byte(raw), &operations); err != nil {
		return hostedImportFieldValues{}, err
	}
	return hostedImportFieldValues{columns: []any{true}, operations: operations}, nil
}
