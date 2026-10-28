package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionmodel "github.com/digitaldrywood/detent/internal/admission/model"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

const storeProjectionBatch = 32

type TypedProjectionMaintainer interface {
	BackfillTypedProjections(context.Context) (int, error)
}

type attemptProjection struct {
	prNumber                              sql.NullInt64
	prHead, prBase, runMode               sql.NullString
	excluded, forgeWait                   sql.NullInt64
	forgeHost, forgeError, forgeCompleted sql.NullString
	turns                                 sql.NullInt64
}

func jsonFields(raw string) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		return nil
	}
	return fields
}

func jsonText(raw json.RawMessage) sql.NullString {
	if len(raw) == 0 || string(raw) == "null" {
		return sql.NullString{}
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return sql.NullString{String: value, Valid: true}
	}
	switch string(raw) {
	case "true":
		return sql.NullString{String: "1", Valid: true}
	case "false":
		return sql.NullString{String: "0", Valid: true}
	}
	return sql.NullString{String: string(raw), Valid: true}
}

func jsonString(raw json.RawMessage) sql.NullString {
	var value string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func jsonInteger(raw json.RawMessage) sql.NullInt64 {
	value := jsonText(raw)
	if !value.Valid {
		return sql.NullInt64{}
	}
	if len(raw) > 0 && raw[0] != '"' && strings.ContainsAny(string(raw), ".eE") {
		number, err := strconv.ParseFloat(string(raw), 64)
		if err == nil || errors.Is(err, strconv.ErrRange) {
			switch {
			case number >= math.MaxInt64:
				return sql.NullInt64{Int64: math.MaxInt64, Valid: true}
			case number <= math.MinInt64:
				return sql.NullInt64{Int64: math.MinInt64, Valid: true}
			}
			return sql.NullInt64{Int64: int64(number), Valid: true}
		}
	}
	text := strings.TrimSpace(value.String)
	end := 0
	if strings.HasPrefix(text, "-") || strings.HasPrefix(text, "+") {
		end++
	}
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	number, err := strconv.ParseInt(text[:end], 10, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		number = 0
	}
	return sql.NullInt64{Int64: number, Valid: true}
}

func projectAttempt(metadata, metrics string) attemptProjection {
	fields := jsonFields(metadata)
	fence := jsonFields(string(fields["historical_completion_fence"]))
	wait := jsonFields(string(fields["forge_wait"]))
	head, base, mode := jsonText(fields["pr_head_sha"]), jsonText(fields["pr_base_sha"]), jsonText(fields["run_mode"])
	head.Valid, base.Valid, mode.Valid = true, true, true
	excluded := int64(0)
	rawExcluded := fence["excluded_from_worker_outcomes"]
	if len(rawExcluded) > 0 && string(rawExcluded) != "null" && string(rawExcluded) != "false" {
		var number float64
		if rawExcluded[0] == '"' || json.Unmarshal(rawExcluded, &number) != nil || number != 0 {
			excluded = 1
		}
	}
	projection := attemptProjection{
		prNumber: jsonInteger(fields["pr_number"]), prHead: head, prBase: base, runMode: mode,
		excluded: sql.NullInt64{Int64: excluded, Valid: true}, forgeWait: sql.NullInt64{Int64: boolInt64(wait != nil), Valid: true},
		forgeHost: jsonString(wait["host"]), forgeError: jsonString(wait["error_class"]), forgeCompleted: jsonString(fields["forge_write_completed_host"]),
	}
	var turns int64
	raw := jsonFields(metrics)["turns"]
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &turns) == nil {
		projection.turns = sql.NullInt64{Int64: turns, Valid: true}
	}
	return projection
}

type phaseProjection struct{ retry, hash, correlation, terminal sql.NullString }

func projectPhase(raw string) phaseProjection {
	fields := jsonFields(raw)
	return phaseProjection{jsonString(fields["retry_identity"]), jsonString(fields["input_hash"]), jsonString(fields["correlation_id"]), jsonString(fields["terminal_outcome"])}
}

func projectGlobalAvailable(raw string) sql.NullInt64 {
	return jsonInteger(jsonFields(raw)["global_available"])
}

func (s *sqliteStore) BackfillTypedProjections(ctx context.Context) (int, error) {
	total := 0
	for _, table := range []string{"work_attempts", "workflow_phase_events", "scheduler_decisions", "backlog_admission_runs"} {
		for range storeProjectionBatch {
			changed, err := s.backfillTypedProjection(ctx, table)
			if err != nil {
				return total, err
			}
			if !changed {
				break
			}
			total++
		}
	}
	return total, nil
}

func (s *sqliteStore) backfillTypedProjection(ctx context.Context, table string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	switch table {
	case "work_attempts":
		var metadata, metrics string
		err = tx.QueryRowContext(ctx, `SELECT id,worker_metadata_json,metrics_json FROM work_attempts WHERE projection_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &metadata, &metrics)
		if err == nil {
			p := projectAttempt(metadata, metrics)
			_, err = tx.ExecContext(ctx, `UPDATE work_attempts SET metadata_pr_number=?,metadata_pr_head_sha=?,metadata_pr_base_sha=?,metadata_run_mode=?,outcome_excluded=?,forge_wait_present=?,forge_wait_host=?,forge_wait_error_class=?,forge_write_completed_host=?,metrics_turns=?,projection_ready=1 WHERE id=?`, p.prNumber, p.prHead, p.prBase, p.runMode, p.excluded, p.forgeWait, p.forgeHost, p.forgeError, p.forgeCompleted, p.turns, id)
		}
	case "workflow_phase_events":
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT id,metadata_json FROM workflow_phase_events WHERE projection_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
		if err == nil {
			p := projectPhase(raw)
			_, err = tx.ExecContext(ctx, `UPDATE workflow_phase_events SET retry_identity=?,input_hash=?,correlation_id=?,terminal_outcome=?,projection_ready=1 WHERE id=?`, p.retry, p.hash, p.correlation, p.terminal, id)
		}
	case "scheduler_decisions":
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT id,capacity_snapshot_json FROM scheduler_decisions WHERE projection_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE scheduler_decisions SET global_available=?,projection_ready=1 WHERE id=?`, projectGlobalAvailable(raw), id)
		}
	case "backlog_admission_runs":
		var raw string
		err = tx.QueryRowContext(ctx, `SELECT id,issues_json FROM backlog_admission_runs WHERE projection_ready IS NULL ORDER BY id LIMIT 1`).Scan(&id, &raw)
		if err == nil {
			var issues []admissionmodel.IssueRecord
			if err = json.Unmarshal([]byte(raw), &issues); err == nil {
				err = writeAdmissionRunIssues(ctx, tx, id, issues)
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE backlog_admission_runs SET projection_ready=1 WHERE id=?`, id)
			}
		}
	default:
		return false, fmt.Errorf("unknown projection table %q", table)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill %s: %w", table, err)
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func writeAdmissionRunIssues(ctx context.Context, tx *sql.Tx, id int64, issues []admissionmodel.IssueRecord) error {
	for position, issue := range issues {
		if _, err := tx.ExecContext(ctx, `INSERT INTO backlog_admission_run_issues(run_id,position,issue_id,fingerprint) VALUES(?,?,?,?)`, id, position, issue.ID, issue.Fingerprint); err != nil {
			return err
		}
	}
	return nil
}

type projectionReadDB struct {
	*sql.DB
	table   string
	columns []string
	values  [][]any
}

func (s *sqliteStore) projectionReader(ctx context.Context, table, projectID string) (sqlc.DBTX, error) {
	var query, columnQuery string
	switch table {
	case "work_attempts":
		query = "SELECT id,worker_metadata_json,metrics_json FROM work_attempts WHERE projection_ready IS NULL ORDER BY id LIMIT 129"
		if projectID != "" {
			query = "SELECT id,worker_metadata_json,metrics_json FROM work_attempts WHERE projection_ready IS NULL AND project_id=? ORDER BY id LIMIT 129"
		}
		columnQuery = "SELECT * FROM work_attempts LIMIT 0"
	case "workflow_phase_events":
		query = "SELECT id,metadata_json FROM workflow_phase_events WHERE projection_ready IS NULL ORDER BY id LIMIT 129"
		if projectID != "" {
			query = "SELECT id,metadata_json FROM workflow_phase_events WHERE projection_ready IS NULL AND project_id=? ORDER BY id LIMIT 129"
		}
		columnQuery = "SELECT * FROM workflow_phase_events LIMIT 0"
	default:
		return nil, errors.New("unsupported projection read table")
	}
	var args []any
	if projectID != "" {
		args = append(args, projectID)
	}
	values, err := s.readProjectionValues(ctx, table, query, args)
	if err != nil {
		return nil, err
	}
	if len(values) == 129 {
		return nil, errors.New("store projections pending maintenance")
	}
	if len(values) == 0 {
		return s.db, nil
	}
	columns, err := s.readProjectionColumns(ctx, columnQuery)
	if err != nil {
		return nil, err
	}
	return &projectionReadDB{DB: s.db, table: table, columns: columns, values: values}, nil
}

func (s *sqliteStore) readProjectionValues(ctx context.Context, table, query string, args []any) ([][]any, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values [][]any
	for rows.Next() {
		var id int64
		var metadata, metrics string
		if table == "work_attempts" {
			err = rows.Scan(&id, &metadata, &metrics)
		} else {
			err = rows.Scan(&id, &metadata)
		}
		if err != nil {
			return nil, err
		}
		if table == "work_attempts" {
			p := projectAttempt(metadata, metrics)
			values = append(values, []any{id, p.prNumber, p.prHead, p.prBase, p.runMode, p.excluded, p.forgeWait, p.forgeHost, p.forgeError, p.forgeCompleted, p.turns, 1, metadata, metrics})
		} else {
			p := projectPhase(metadata)
			values = append(values, []any{id, p.retry, p.hash, p.correlation, p.terminal, 1, metadata})
		}
	}
	return values, rows.Err()
}

func (s *sqliteStore) readProjectionColumns(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func (r *projectionReadDB) projectQuery(query string, args []any) (string, []any) {
	fields := []string{"id", "metadata_pr_number", "metadata_pr_head_sha", "metadata_pr_base_sha", "metadata_run_mode", "outcome_excluded", "forge_wait_present", "forge_wait_host", "forge_wait_error_class", "forge_write_completed_host", "metrics_turns", "projection_ready", "source_metadata", "source_metrics"}
	if r.table == "workflow_phase_events" {
		fields = []string{"id", "retry_identity", "input_hash", "correlation_id", "terminal_outcome", "projection_ready", "source_metadata"}
	}
	args = append([]any(nil), args...)
	var records []string
	for _, values := range r.values {
		placeholders := make([]string, 0, len(values))
		for _, value := range values {
			args = append(args, value)
			placeholders = append(placeholders, "?"+strconv.Itoa(len(args)))
		}
		records = append(records, "("+strings.Join(placeholders, ",")+")")
	}
	selected := make([]string, 0, len(r.columns))
	for _, column := range r.columns {
		expression := "source." + column
		if slices.Contains(fields[1:], column) {
			expression = "CASE WHEN source.projection_ready IS NULL THEN fallback." + column + " ELSE source." + column + " END AS " + column
		}
		selected = append(selected, expression)
	}
	projected := "projected_" + r.table
	query = regexp.MustCompile(` INDEXED BY `+r.table+`_\w+`).ReplaceAllString(query, "")
	query = regexp.MustCompile(`\b`+r.table+`\b`).ReplaceAllString(query, projected)
	sourceMatch := " AND fallback.source_metadata=source.metadata_json"
	if r.table == "work_attempts" {
		sourceMatch = " AND fallback.source_metadata=source.worker_metadata_json AND fallback.source_metrics=source.metrics_json"
	}
	prefix := "projection_fallback(" + strings.Join(fields, ",") + ") AS (VALUES " + strings.Join(records, ",") + "), " + projected + " AS NOT MATERIALIZED (SELECT " + strings.Join(selected, ",") + " FROM " + r.table + " AS source LEFT JOIN projection_fallback AS fallback ON fallback.id=source.id" + sourceMatch + ")"
	if position := strings.Index(query, "WITH "); position >= 0 {
		query = query[:position] + "WITH " + prefix + ", " + query[position+5:]
	} else {
		query = "WITH " + prefix + " " + query
	}
	return query, args
}

func (r *projectionReadDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, args = r.projectQuery(query, args)
	return r.DB.QueryContext(ctx, query, args...)
}

func (r *projectionReadDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	query, args = r.projectQuery(query, args)
	return r.DB.QueryRowContext(ctx, query, args...)
}

func (s *sqliteStore) attemptReadQueries(ctx context.Context, projectID string) (*sqlc.Queries, error) {
	reader, err := s.projectionReader(ctx, "work_attempts", projectID)
	if err != nil {
		return nil, err
	}
	if reader == s.db {
		return s.queries, nil
	}
	return sqlc.New(reader), nil
}
