package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	return ctx, nil
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
