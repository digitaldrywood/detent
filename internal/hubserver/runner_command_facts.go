package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func insertNativeCommandReceipt(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, actor, operation, key, hash, raw, at string) (sql.Result, error) {
	pending, request, err := commandResponseFacts(raw)
	if err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending,response_request_id) VALUES(?,?,?,?,?,?,?,?,?)`, organization, actor, operation, key, hash, raw, at, pending, request)
}

func insertNativePayloadCommandReceipt(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, actor, operation, key, hash, raw, at string) (sql.Result, error) {
	pending, request, err := commandResponseFacts(raw)
	if err != nil {
		return nil, err
	}
	object, err := storedPayloadKey(ctx, string(organization), []byte(raw))
	if err != nil {
		return nil, err
	}
	return tx.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,response_object_key,created_at,response_pending,response_request_id) VALUES(?,?,?,?,?,'{}',?,?,?,?)`, organization, actor, operation, key, hash, object, at, pending, request)
}

func readUrgentCommandReceipt(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, request string) (string, error) {
	var known string
	err := q.QueryRowContext(ctx, `SELECT response_json FROM native_commands WHERE organization_id=? AND operation='urgent_runner_update' AND response_request_id=? AND response_pending IS NOT NULL LIMIT 1`, organization, request).Scan(&known)
	if err == nil {
		return known, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := q.QueryContext(ctx, `SELECT response_json FROM native_commands WHERE organization_id=? AND operation='urgent_runner_update' AND response_pending IS NULL ORDER BY rowid LIMIT ?`, organization, runnerFactsRowsPerTick+1)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > runnerFactsRowsPerTick {
			return "", errProjectServiceUnavailable
		}
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return "", err
		}
		_, identity, err := commandResponseFacts(raw)
		if err != nil {
			return "", err
		}
		if identity == request {
			return raw, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", sql.ErrNoRows
}

func readCompletedConfigurationReceipt(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, operations []string) (string, string, error) {
	queries := make([]string, 0, len(operations))
	var args []any
	for _, operation := range operations {
		queries = append(queries, `SELECT * FROM (SELECT response_json,operation,response_pending,created_at,rowid AS receipt_rowid FROM native_commands WHERE organization_id=? AND operation=? AND (response_pending=0 OR response_pending IS NULL) ORDER BY created_at DESC,rowid DESC LIMIT ?)`)
		args = append(args, organization, operation, runnerFactsRowsPerTick+1)
	}
	args = append(args, runnerFactsRowsPerTick+1)
	rows, err := q.QueryContext(ctx, `SELECT response_json,operation,response_pending FROM (`+strings.Join(queries, ` UNION ALL `)+`) ORDER BY created_at DESC,receipt_rowid DESC LIMIT ?`, args...)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > runnerFactsRowsPerTick {
			return "", "", errProjectServiceUnavailable
		}
		var raw, operation string
		var pending sql.NullBool
		if err := rows.Scan(&raw, &operation, &pending); err != nil {
			return "", "", err
		}
		if !pending.Valid {
			value, _, err := commandResponseFacts(raw)
			if err != nil {
				return "", "", err
			}
			pending.Bool = value
		}
		if !pending.Bool {
			return raw, operation, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return "", "", sql.ErrNoRows
}
