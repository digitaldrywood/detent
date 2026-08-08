package operatortool

import (
	"encoding/json"
	"fmt"
)

const (
	BoardState       = "board_state"
	FleetHealth      = "fleet_health"
	TelemetryUsage   = "telemetry_usage"
	RecentActivity   = "recent_activity"
	ExplainItem      = "explain_item"
	DefaultItemLimit = 100
	MaxItemLimit     = 200
	MaxArgumentBytes = 64 * 1024
	MaxResultBytes   = 256 * 1024
)

type Definition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

func Catalog() []Definition {
	limitedSchema := fmt.Sprintf(`{"type":"object","properties":{"project_id":{"type":"string"},"state":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":%d}},"additionalProperties":false}`, MaxItemLimit)
	activitySchema := fmt.Sprintf(`{"type":"object","properties":{"project_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":%d}},"additionalProperties":false}`, MaxItemLimit)
	return []Definition{
		definition(BoardState, "Read live board items, lanes, priorities, blockers, and active run identity. Use this before answering board questions or proposing item actions.", limitedSchema),
		definition(FleetHealth, "Read live fleet health, capacity outages, failure breakers, rate limits, refresh state, and running counts.", `{"type":"object","properties":{},"additionalProperties":false}`),
		definition(TelemetryUsage, "Read live token, spend, throughput, and per-project usage telemetry.", `{"type":"object","properties":{"project_id":{"type":"string"}},"additionalProperties":false}`),
		definition(RecentActivity, "Read recent events and completed work retained in the current live telemetry snapshot, including merge timestamps. This is live-only activity, not the durable issue activity stream.", activitySchema),
		definition(ExplainItem, "Explain an issue's current lane, latest transition reason, eligibility, active or latest attempt, sessions, pull request, required gate, freshness, and evidence from the versioned issue explanation read model.", `{"type":"object","required":["project_id","reference"],"properties":{"project_id":{"type":"string","minLength":1},"reference":{"type":"string","minLength":1}},"additionalProperties":false}`),
	}
}

func Lookup(name string) (Definition, bool) {
	for _, definition := range Catalog() {
		if definition.Name == name {
			return definition, true
		}
	}
	return Definition{}, false
}

func definition(name string, description string, schema string) Definition {
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}
