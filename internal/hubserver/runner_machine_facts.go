package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type machineSpriteFacts struct {
	Name         *string `json:"sprite_name"`
	WokenAt      *string `json:"sprite_woken_at"`
	WakeFailedAt *string `json:"sprite_wake_failed_at"`
}

func readMachineSpriteFacts(ctx context.Context, q hubapi.NativeQueryer, organization tracker.OrganizationID, machine tracker.MachineID) (machineSpriteFacts, error) {
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

func resolveMachineSpriteFacts(facts machineSpriteFacts, legacy sql.NullString) (machineSpriteFacts, error) {
	if legacy.Valid {
		facts = machineSpriteFacts{}
		if err := json.Unmarshal([]byte(legacy.String), &facts); err != nil {
			return facts, err
		}
	}
	return facts, nil
}
