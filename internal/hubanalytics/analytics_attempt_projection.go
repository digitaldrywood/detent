package hubanalytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const AttemptPayload = `COALESCE(NULLIF(r.payload_object_key,''),NULLIF(r.data_json,'{}'),(SELECT COALESCE(NULLIF(n.payload_object_key,''),n.data_json) FROM main.native_attempts n WHERE n.organization_id=r.organization_id AND n.project_id=r.project_id AND n.id=r.dimension),'{}')`

func RefreshAttempts(ctx context.Context, tx *sql.Tx, statements *Statements, organization, project, item string) error {
	query := `SELECT id,work_item_id,lease_id,status,started_at,updated_at,CASE WHEN payload_object_key='' THEN data_json ELSE '{}' END,payload_object_key,report_failed_at,coalesce(nullif(role,''),execution_role,'') FROM native_attempts WHERE organization_id=? AND project_id=?`
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
		id, payload, key, role string
		hour                   int64
	}
	var attempts []projectedAttempt
	for rows.Next() {
		var id, workItem, lease, status, started, updated, raw, key, role string
		var failed sql.NullString
		var observation AttemptProjection
		if err := rows.Scan(&id, &workItem, &lease, &status, &started, &updated, &raw, &key, &failed, &role); err != nil {
			return fmt.Errorf("scan analytics attempt source: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return err
		}
		if role == "" {
			role = observation.Runtime.Identity.Role
			if role == "" {
				role = observation.Identity.Role
			}
		}
		if !failed.Valid {
			failed = FailureTime(observation.Runtime.Landing, status, updated)
		}
		payload, err := ProjectAttempt(raw, id, workItem, lease, status, started, updated, failed)
		if err != nil {
			return err
		}
		at, err := parseTimeValue(started)
		if err != nil {
			return err
		}
		attempts = append(attempts, projectedAttempt{id, payload, key, role, at.Unix() / 3600 * 3600})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, attempt := range attempts {
		if err := writeAnalyticsSample(ctx, tx, organization, project, attempt.hour, "attempt", attempt.id, 1, attempt.payload); err != nil {
			return fmt.Errorf("write analytics attempt projection: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE analytics_hourly_rollups SET payload_object_key=?,attempt_role=? WHERE organization_id=? AND project_id=? AND metric='attempt' AND dimension=?", attempt.key, attempt.role, organization, project, attempt.id); err != nil {
			return err
		}
	}
	return nil
}

type command struct {
	TimingStage     string                 `json:"timing_stage,omitempty"`
	ReceiptID       string                 `json:"receipt_id,omitempty"`
	Execution       string                 `json:"execution,omitempty"`
	ReusedReceiptID string                 `json:"reused_receipt_id,omitempty"`
	StartedAt       time.Time              `json:"started_at,omitzero"`
	FinishedAt      time.Time              `json:"finished_at,omitzero"`
	Pipeline        *gate.PipelineEvidence `json:"pipeline,omitzero"`
	DurationNS      int64                  `json:"duration_ns,omitempty"`
	Command         string                 `json:"command"`
	HeadSHA         string                 `json:"head_sha"`
	TreeSHA         string                 `json:"tree_sha"`
	ExitCode        int                    `json:"exit_code"`
}

type AttemptLanding struct {
	Pipeline        []gate.PipelineTiming `json:"pipeline,omitempty"`
	PipelineDropped int                   `json:"pipeline_dropped,omitempty"`
	Gate            *command              `json:"gate,omitempty"`
	FromState       string                `json:"from_state,omitempty"`
	TargetState     string                `json:"target_state,omitempty"`
	GateFailed      bool                  `json:"gate_failed,omitempty"`
	Rebased         bool                  `json:"rebased,omitempty"`
	ChangeID        string                `json:"change_id,omitempty"`
	VersionID       string                `json:"version_id,omitempty"`
	HeadSHA         string                `json:"head_sha,omitempty"`
	Landed          bool                  `json:"landed"`
	MergeSHA        string                `json:"merge_sha,omitempty"`
	BaseRef         string                `json:"base_ref,omitempty"`
	BaseSHA         string                `json:"base_sha,omitempty"`
	Method          string                `json:"method,omitempty"`
	RefusalKind     string                `json:"refusal_kind,omitempty"`
	ObservedAt      time.Time             `json:"observed_at"`
}

type Phases struct {
	Items     []tracker.NativePhase
	Malformed bool
}

func (p *Phases) UnmarshalJSON(raw []byte) error {
	if json.Unmarshal(raw, &p.Items) != nil {
		p.Items, p.Malformed = nil, true
	}
	return nil
}

func (p Phases) MarshalJSON() ([]byte, error) {
	if p.Malformed {
		return []byte(`"malformed"`), nil
	}
	return json.Marshal(p.Items)
}

type attemptRuntime struct {
	Phases          Phases                           `json:"phases"`
	PhasesDropped   int                              `json:"phases_dropped"`
	Activity        *workflowmetrics.ActivityProfile `json:"activity,omitempty"`
	Identity        agentidentity.Identity           `json:"identity,omitzero"`
	Landing         *AttemptLanding                  `json:"landing,omitempty"`
	Pipeline        []gate.PipelineTiming            `json:"pipeline,omitempty"`
	PipelineDropped int                              `json:"pipeline_dropped,omitempty"`
	Validation      *command                         `json:"validation,omitempty"`
}

type AttemptProjection struct {
	ID         string                 `json:"id"`
	WorkItemID string                 `json:"work_item_id"`
	LeaseID    string                 `json:"lease_id"`
	RunnerID   string                 `json:"runner_id,omitempty"`
	Status     string                 `json:"status"`
	StartedAt  string                 `json:"started_at"`
	UpdatedAt  string                 `json:"updated_at"`
	FailedAt   *string                `json:"failed_at"`
	Identity   agentidentity.Identity `json:"identity,omitzero"`
	Runtime    attemptRuntime         `json:"runtime"`
}

func ProjectAttempt(raw, id, item, lease, status, started, updated string, failed sql.NullString) (string, error) {
	var payload AttemptProjection
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", fmt.Errorf("decode analytics attempt source: %w", err)
	}
	if activity := payload.Runtime.Activity; activity != nil && ValidActivity(*activity) {
		public := workflowmetrics.PublicActivityProfile(*activity)
		payload.Runtime.Activity = &public
	}
	payload.ID, payload.WorkItemID, payload.LeaseID = id, item, lease
	payload.Status, payload.StartedAt, payload.UpdatedAt = status, started, updated
	payload.FailedAt = nil
	if failed.Valid {
		payload.FailedAt = &failed.String
	}
	encoded, err := json.Marshal(payload)
	return string(encoded), err
}

func FailureTime(landing *AttemptLanding, status, updated string) sql.NullString {
	refusal := landing != nil && (landing.RefusalKind == "conflict" || landing.RefusalKind == "base_protected")
	gate := landing != nil && landing.GateFailed
	if status != "failed" && status != "interrupted" && !refusal && !gate {
		return sql.NullString{}
	}
	at := updated
	if landing != nil && ((refusal && !landing.Landed) || gate) && !landing.ObservedAt.IsZero() {
		at = formatHubTime(landing.ObservedAt)
	}
	return sql.NullString{String: at, Valid: true}
}
