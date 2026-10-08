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
		definition(BoardState, "Read live board items, lanes, priorities, blockers, and active run identity. Native hubs return bounded board inventory and live-worker observations; queued inventory includes held work, not dispatch readiness. Use explain_item for current item eligibility.", limitedSchema),
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
		definition(LocalProjectConfiguration, "Read the selected configuration revision, effective localhost permission and complete policy candidates through the local or enrolled runner owner. policy_mismatch means selected_policy differs from effective_policy. Pass the entire selected_policy object unchanged as approve_project_policy input.policy; omitted fields can invalidate its identity. Provenance is redacted; a stopped owner is explicit.", localProjectSchema(LocalProjectConfiguration)),
		definition("apply_local_project_policy", "Apply an approved exact policy to a settled project through its configuration owner, restoring its prior running, paused or draining state. The optional existing allow_local_binding setting includes other localhost services. Cloud requires runner_id and expected_runner_revision. Requires current administrator authority.", localProjectSchema("apply_local_project_policy")),
		definition("resume_local_project", "Resume the selected paused project through its existing configuration owner. Cloud requires runner_id and expected_runner_revision. Requires current administrator scope and runner administration (manage_runner).", localProjectSchema("resume_local_project")),
		definition("drain_local_project", "Drain only the selected local project through its existing owner, finishing active work and retaining deferred completion authority. Requires current administrator authority.", localProjectSchema("drain_local_project")),
		definition("detach_local_project", "Remove only the selected local registration after settled work and verified mapped Cloud cutover. Saved removal awaits the existing reload receipt. Requires current administrator authority; never starts a board.", localProjectSchema("detach_local_project")),
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
	for _, catalog := range [][]Definition{CommandCatalog(), AttachmentCatalog(), ProjectCatalog(), LocalProjectCatalog(), WorkspaceCatalog(), OperatorChatCatalog(), ChangeCatalog(), AdministrationCatalog(), HostedContextCatalog()} {
		definitions = append(definitions, catalog...)
	}
	return definitions
}
