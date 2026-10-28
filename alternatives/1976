package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type machineSpriteFacts struct {
	Name         *string `json:"sprite_name"`
	WokenAt      *string `json:"sprite_woken_at"`
	WakeFailedAt *string `json:"sprite_wake_failed_at"`
}

func readMachineSpriteFacts(ctx context.Context, q nativeQueryer, organization tracker.OrganizationID, machine tracker.MachineID) (machineSpriteFacts, error) {
	var facts machineSpriteFacts
	var legacy sql.NullString
	err := q.QueryRowContext(ctx, `SELECT sprite_name,sprite_woken_at,sprite_wake_failed_at,CASE WHEN machine_facts_ready IS NULL THEN capabilities_json END FROM machines WHERE organization_id=? AND id=?`, organization, machine).Scan(&facts.Name, &facts.WokenAt, &facts.WakeFailedAt, &legacy)
	if err == nil && legacy.Valid {
		facts, err = resolveMachineSpriteFacts(facts, legacy)
	}
	return facts, err
}

func spriteFactValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func writeMachineSpriteFacts(ctx context.Context, tx *sql.Tx, machine tracker.MachineID, raw string) error {
	var facts machineSpriteFacts
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE machines SET sprite_name=?,sprite_woken_at=?,sprite_wake_failed_at=?,machine_facts_ready=1 WHERE id=?`, facts.Name, facts.WokenAt, facts.WakeFailedAt, machine)
	return err
}

func updateMachineSpriteFact(ctx context.Context, tx *sql.Tx, machine tracker.MachineID, field, value string) error {
	var raw string
	var facts machineSpriteFacts
	var ready sql.NullBool
	if err := tx.QueryRowContext(ctx, `SELECT capabilities_json,sprite_name,sprite_woken_at,sprite_wake_failed_at,machine_facts_ready FROM machines WHERE id=?`, machine).Scan(&raw, &facts.Name, &facts.WokenAt, &facts.WakeFailedAt, &ready); err != nil {
		return err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if payload == nil {
		payload = make(map[string]json.RawMessage)
	}

	if ready.Valid && ready.Bool {
		for key, current := range map[string]*string{"sprite_name": facts.Name, "sprite_woken_at": facts.WokenAt, "sprite_wake_failed_at": facts.WakeFailedAt} {
			if current == nil {
				delete(payload, key)
				continue
			}
			value, err := json.Marshal(*current)
			if err != nil {
				return err
			}
			payload[key] = value
		}
	}
	payload[field] = encoded
	updated, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE machines SET capabilities_json=? WHERE id=?`, string(updated), machine); err != nil {
		return err
	}
	return writeMachineSpriteFacts(ctx, tx, machine, string(updated))
}

func storeRunnerRoutingFacts(ctx context.Context, tx *sql.Tx, runner string, raw string) error {
	var settings runnerSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return err
	}
	return writeRunnerRoutingFacts(ctx, tx, runner, settings)
}

func writeRunnerRoutingFacts(ctx context.Context, tx *sql.Tx, runner string, settings runnerSettings) error {
	if err := storeConfigurationCommandFacts(ctx, tx, runner, settings.ProjectConfigurationCommand); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runner_project_ranks WHERE runner_id=?`, runner); err != nil {
		return err
	}
	for project, rank := range settings.ProjectRanks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO runner_project_ranks(runner_id,project_id,rank) VALUES(?,?,?)`, runner, project, rank); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE runner_identities SET routing_facts_ready=1 WHERE id=?`, runner)
	return err
}

func writeAttemptUsageFacts(ctx context.Context, tx *sql.Tx, id string, data tracker.NativeRunData) error {
	var peak, user, system, wall any
	if usage := data.ProcessUsage; usage != nil {
		peak, user, system, wall = usage.PeakMemoryBytes, usage.UserCPUSeconds, usage.SystemCPUSeconds, usage.WallSeconds
	}
	_, err := tx.ExecContext(ctx, `UPDATE native_attempts SET machine_id=?,usage_facts_ready=1,process_peak_memory_bytes=?,process_user_cpu_seconds=?,process_system_cpu_seconds=?,process_wall_seconds=? WHERE id=?`, data.MachineID, peak, user, system, wall, id)
	return err
}

func commandResponseFacts(raw string) (bool, string, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		if !json.Valid([]byte(raw)) {
			return false, "", err
		}
		return false, "", nil
	}
	var pending bool
	if value := response["pending"]; len(value) > 0 {
		if err := json.Unmarshal(value, &pending); err != nil {
			var number float64
			pending = json.Unmarshal(value, &number) != nil || number != 0
		}
	}
	var request struct {
		ID string `json:"id"`
	}
	if value := response["request"]; len(value) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) == nil {
			if value := fields["id"]; len(value) > 0 {
				if err := json.Unmarshal(value, &request.ID); err != nil {
					request.ID = ""
				}
			}
		}
	}
	return pending, request.ID, nil
}

func resolveMachineSpriteFacts(facts machineSpriteFacts, legacy sql.NullString) (machineSpriteFacts, error) {
	if legacy.Valid {
		facts = machineSpriteFacts{}
		if err := json.Unmarshal([]byte(legacy.String), &facts); err != nil {
			return facts, err
		}
	}
	return facts, nil
}
