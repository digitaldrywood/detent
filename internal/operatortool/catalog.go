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
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations Annotations     `json:"annotations"`
	Meta        ToolMetadata    `json:"_meta"`
}

type ToolMetadata struct {
	Toolset string `json:"detent/toolset"`
}

func toolset(name string) ToolMetadata {
	group := "actions"
	switch name {
	case BoardState, ExplainItem:
		group = "board"
	case FleetHealth:
		group = "fleet"
	case TelemetryUsage, RecentActivity:
		group = "telemetry"
	case ConnectionInfo:
		group = "connection"
	}
	return ToolMetadata{Toolset: group}
}

type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
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
	for _, definition := range Registry() {
		if definition.Name == name {
			return definition, true
		}
	}
	return Definition{}, false
}

func definition(name string, description string, schema string) Definition {
	return Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), Annotations: Annotations{ReadOnly: true, Idempotent: true}, Meta: toolset(name)}
}

func LocalProjectCatalog() []Definition {
	result := []Definition{
		definition(LocalProjectConfiguration, "Read the actual selected local configuration revision and effective workflow policy with redacted provenance. Cloud routing is a separate authority; a stopped owner is explicit.", localProjectSchema(LocalProjectConfiguration)),
		definition("apply_local_project_policy", "Apply an already-approved exact policy from the configured committed workflow to a paused, settled local project through its configuration owner. Requires operator approval.", localProjectSchema("apply_local_project_policy")),
		definition("drain_local_project", "Drain only the selected local project through its existing owner, finishing active work and retaining deferred completion authority. Requires operator approval.", localProjectSchema("drain_local_project")),
		definition("detach_local_project", "Remove only the selected local registration after settled work and verified mapped Cloud cutover. Saved removal awaits the existing reload receipt. Requires operator approval; never starts a board.", localProjectSchema("detach_local_project")),
	}
	for i := range result {
		result[i].Meta = ToolMetadata{Toolset: "local_projects"}
		if result[i].Name != LocalProjectConfiguration {
			result[i].Annotations = Annotations{Destructive: true, Idempotent: true, OpenWorld: true}
		}
	}
	return result
}

// Registry is the canonical protocol registry; deployments filter by application availability.
func Registry() []Definition {
	definitions := append(Catalog(), WorkReadCatalog()...)
	for _, catalog := range [][]Definition{CommandCatalog(), AttachmentCatalog(), ProjectCatalog(), LocalProjectCatalog(), WorkspaceCatalog(), OperatorChatCatalog(), ChangeCatalog(), AdministrationCatalog()} {
		definitions = append(definitions, catalog...)
	}
	return definitions
}
