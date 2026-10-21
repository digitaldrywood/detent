package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const analyticsAttemptUpsert = `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES(?,?,?,'attempt',?,1,?) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`

func refreshAnalyticsAttempts(ctx context.Context, tx *sql.Tx, statements analyticsStatements, organization, project, item string) error {
	query := `SELECT id,work_item_id,lease_id,status,started_at,updated_at,data_json,` + analyticsAttemptFailedAt + ` FROM native_attempts WHERE organization_id=? AND project_id=?`
	args := []any{organization, project}
	if item != "" {
		query += " AND work_item_id=?"
		args = append(args, item)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read analytics attempt sources: %w", err)
	}
	defer rows.Close()
	type projectedAttempt struct {
		id, payload string
		hour        int64
	}
	var attempts []projectedAttempt
	for rows.Next() {
		var id, workItem, lease, status, started, updated, raw string
		var failed sql.NullString
		if err := rows.Scan(&id, &workItem, &lease, &status, &started, &updated, &raw, &failed); err != nil {
			return fmt.Errorf("scan analytics attempt source: %w", err)
		}
		payload, err := projectAnalyticsAttempt(raw, id, workItem, lease, status, started, updated, failed)
		if err != nil {
			return err
		}
		at, err := parseTimeValue(started)
		if err != nil {
			return err
		}
		attempts = append(attempts, projectedAttempt{id, payload, at.Unix() / 3600 * 3600})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, attempt := range attempts {
		if _, err := statements.exec(ctx, tx, analyticsAttemptUpsert, organization, project, attempt.hour, attempt.id, attempt.payload); err != nil {
			return fmt.Errorf("write analytics attempt projection: %w", err)
		}
	}
	return nil
}

func projectAnalyticsAttempt(raw, id, item, lease, status, started, updated string, failed sql.NullString) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", fmt.Errorf("decode analytics attempt source: %w", err)
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	var runtime map[string]json.RawMessage
	if json.Unmarshal(payload["runtime"], &runtime) == nil && runtime != nil {
		var activity workflowmetrics.ActivityProfile
		if json.Unmarshal(runtime["activity"], &activity) == nil && validAnalyticsActivity(activity) {
			activity = workflowmetrics.PublicActivityProfile(activity)
			encoded, err := json.Marshal(activity)
			if err != nil {
				return "", fmt.Errorf("encode analytics activity: %w", err)
			}
			runtime["activity"] = encoded
		}
		for _, key := range []string{"validation", "landing"} {
			var receipt map[string]json.RawMessage
			if json.Unmarshal(runtime[key], &receipt) != nil || receipt == nil {
				continue
			}
			delete(receipt, "output")
			if key == "landing" {
				var gate map[string]json.RawMessage
				if json.Unmarshal(receipt["gate"], &gate) == nil && gate != nil {
					delete(gate, "output")
					encoded, err := json.Marshal(gate)
					if err != nil {
						return "", err
					}
					receipt["gate"] = encoded
				}
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				return "", err
			}
			runtime[key] = encoded
		}
		encoded, err := json.Marshal(runtime)
		if err != nil {
			return "", err
		}
		payload["runtime"] = encoded
	}
	for key, value := range map[string]string{"id": id, "work_item_id": item, "lease_id": lease, "status": status, "started_at": started, "updated_at": updated} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		payload[key] = encoded
	}
	payload["failed_at"] = json.RawMessage("null")
	if failed.Valid {
		encoded, err := json.Marshal(failed.String)
		if err != nil {
			return "", err
		}
		payload["failed_at"] = encoded
	}
	encoded, err := json.Marshal(payload)
	return string(encoded), err
}
