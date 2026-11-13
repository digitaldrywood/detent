package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storeAdmissionFacts(ctx context.Context, tx *sql.Tx, runner string, project tracker.ProjectID, observed tracker.NativeAdmissionObservation) error {
	c := observed.Context
	_, err := tx.ExecContext(ctx, `INSERT INTO runner_admission_observations(runner_id,project_id,runner_revision,received_at,observed_at,policy_id,last_refresh_at,refresh_duration) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(runner_id,project_id) DO UPDATE SET runner_revision=excluded.runner_revision,received_at=excluded.received_at,observed_at=excluded.observed_at,policy_id=excluded.policy_id,last_refresh_at=excluded.last_refresh_at,refresh_duration=excluded.refresh_duration`, runner, project, observed.RunnerRevision, projectionTime(observed.ReceivedAt), projectionTime(c.ObservedAt), c.PolicyID, projectionTime(c.LastRefreshAt), c.RefreshDuration)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_admission_selectors WHERE runner_id=? AND project_id=?`, runner, project); err != nil {
		return err
	}
	for kind, values := range map[string][]string{"workflow": c.WorkflowStates, "author": c.Authors, "assignee": c.Assignees, "include": c.LabelInclude, "exclude": c.LabelExclude} {
		for position, value := range values {
			if _, err := tx.ExecContext(ctx, `INSERT INTO runner_admission_selectors(runner_id,project_id,kind,position,value) VALUES(?,?,?,?,?)`, runner, project, kind, position, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func readAdmissionFacts(ctx context.Context, q hubapi.NativeQueryer, runner string, project tracker.ProjectID, machine tracker.MachineID, organization tracker.OrganizationID) (tracker.NativeAdmissionObservation, error) {
	var observation tracker.NativeAdmissionObservation
	var received, observed, refreshed string
	c := &observation.Context
	err := q.QueryRowContext(ctx, `SELECT runner_revision,received_at,observed_at,policy_id,last_refresh_at,refresh_duration FROM runner_admission_observations WHERE runner_id=? AND project_id=?`, runner, project).Scan(&observation.RunnerRevision, &received, &observed, &c.PolicyID, &refreshed, &c.RefreshDuration)
	if errors.Is(err, sql.ErrNoRows) {
		var raw string
		if err := q.QueryRowContext(ctx, `SELECT capabilities_json FROM machines WHERE id=? AND organization_id=?`, machine, organization).Scan(&raw); err != nil {
			return observation, err
		}
		var payload struct {
			Admission map[string]map[tracker.ProjectID]json.RawMessage `json:"native_admission"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			return observation, err
		}
		value := payload.Admission[runner][project]
		if len(value) > 0 && len(value) <= 8192 {
			if err := json.Unmarshal(value, &observation); err != nil {
				return tracker.NativeAdmissionObservation{}, nil
			}
		}
		return observation, nil
	}
	if err != nil {
		return observation, err
	}
	if received != "" {
		observation.ReceivedAt, err = hubapi.ParseTimeValue(received)
		if err != nil {
			return observation, err
		}
	}
	if observed != "" {
		c.ObservedAt, err = hubapi.ParseTimeValue(observed)
		if err != nil {
			return observation, err
		}
	}
	if refreshed != "" {
		c.LastRefreshAt, err = hubapi.ParseTimeValue(refreshed)
		if err != nil {
			return observation, err
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT kind,value FROM runner_admission_selectors WHERE runner_id=? AND project_id=? ORDER BY kind,position`, runner, project)
	if err != nil {
		return observation, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		if err := rows.Scan(&kind, &value); err != nil {
			return observation, err
		}
		switch kind {
		case "workflow":
			c.WorkflowStates = append(c.WorkflowStates, value)
		case "author":
			c.Authors = append(c.Authors, value)
		case "assignee":
			c.Assignees = append(c.Assignees, value)
		case "include":
			c.LabelInclude = append(c.LabelInclude, value)
		case "exclude":
			c.LabelExclude = append(c.LabelExclude, value)
		}
	}
	return observation, rows.Err()
}

func storeLegacyAdmission(ctx context.Context, tx *sql.Tx, scope nativeScope, raw string) error {
	var document string
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_json FROM machines WHERE id=? AND organization_id=?`, scope.credential.Runner.MachineID, scope.organization).Scan(&document); err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &payload); err != nil {
		return err
	}
	var admissions map[string]map[tracker.ProjectID]json.RawMessage
	if value := payload["native_admission"]; len(value) > 0 {
		if err := json.Unmarshal(value, &admissions); err != nil {
			return err
		}
	}
	if admissions == nil {
		admissions = make(map[string]map[tracker.ProjectID]json.RawMessage)
	}
	runner := scope.credential.Runner.RunnerID
	if admissions[runner] == nil {
		admissions[runner] = make(map[tracker.ProjectID]json.RawMessage)
	}
	admissions[runner][scope.project] = json.RawMessage(raw)
	encoded, err := json.Marshal(admissions)
	if err != nil {
		return err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}
	payload["native_admission"] = encoded
	encoded, err = json.Marshal(payload)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE machines SET capabilities_json=? WHERE id=? AND organization_id=?`, string(encoded), scope.credential.Runner.MachineID, scope.organization)
	return requireRunnerUpdate(result, err)
}
