package operatortool

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

const (
	InstanceHealth           = "health"
	AIDebugPrompt            = "ai_debug_prompt"
	NativeCapabilities       = "native_capabilities"
	OutboxHealth             = "outbox_health"
	RunnerFleet              = "runner_fleet"
	OperationsReport         = "operations_report"
	Dashboard                = "dashboard"
	HealthDashboard          = "health_dashboard"
	DiagnosticsDashboard     = "diagnostics_dashboard"
	AnalyticsDashboard       = "analytics_dashboard"
	TimeSeries               = "time_series"
	Reports                  = "reports"
	Refresh                  = "refresh"
	CapacityClear            = "capacity_clear"
	TrackerAvailabilityClear = "tracker_availability_clear"
	ForgeAvailabilityClear   = "forge_availability_clear"
	FailureBreakerCanary     = "failure_breaker_canary"
	UpdateApply              = "update_apply"
	GetRunnerUpdate          = "get_runner_update"
	GetUrgentRunnerUpdate    = "get_urgent_runner_update"
	MarkUrgentRunnerUpdate   = "mark_urgent_runner_update"
	ProgressCredit           = "issue_progress_credit"
	AcknowledgeWarnings      = "acknowledge_staleness_warnings"
	RecoverAttempt           = "recover_work_attempt"
	UpdateFleetRunner        = "update_fleet_runner"
	UpdateFleetHost          = "update_fleet_host"
	CreateRunnerEnrollment   = "create_runner_enrollment"
	RevokeRunnerEnrollment   = "revoke_runner_enrollment"
	RevokeRunnerIdentity     = "revoke_runner_identity"
	GetRunnerRouting         = "get_runner_routing"
	ListRunnerRouting        = "list_runner_routing"
	UpdateRunnerRouting      = "update_runner_routing"
	GetRunnerCapacity        = "get_runner_capacity"
	UpdateRunnerCapacity     = "update_runner_capacity"
	UpdateRunnerHost         = "update_runner_host"
	HostedFleet              = "hosted_fleet"
	GitHubRequestCounts      = "github_request_counts"
)

const boundedID = `{"type":"string","minLength":1,"maxLength":256}`
const boundedList = `{"type":"array","minItems":1,"maxItems":100,"items":` + boundedID + `}`

// FleetCatalog describes operator application operations only. Registration,
// claims, credential renewal/rotation, leases and heartbeats remain worker APIs.
func FleetCatalog() []Definition {
	var definitions []Definition
	add := func(name, description, fields, required string, read, material bool) {
		properties := `"project_id":` + boundedID
		if !read {
			properties += `,"request_id":{"type":"string","minLength":1,"maxLength":128}`
			required = strings.Trim(required+`,"request_id"`, ",")
		}
		if fields != "" {
			properties += "," + fields
		}
		if required != "" {
			required = `,"required":[` + required + `]`
		}
		definitions = append(definitions, Definition{Name: name, Description: description, InputSchema: json.RawMessage(`{"type":"object","properties":{` + properties + `}` + required + `,"additionalProperties":false}`), Annotations: Annotations{ReadOnly: read, Destructive: material, Idempotent: true, OpenWorld: !read || name == RunnerFleet}, Meta: ToolMetadata{Toolset: "fleet"}})
	}
	for _, name := range []string{Dashboard, HealthDashboard, DiagnosticsDashboard} {
		description := "Read the current dashboard application snapshot, projected to current project grants."
		if name == Dashboard {
			description += " Native hubs return bounded board inventory and live-worker observations; queued inventory includes held work, not dispatch readiness. Use explain_item for current item eligibility."
		}
		add(name, description, `"limit":{"type":"integer","minimum":1,"maximum":200}`, "", true, false)
	}
	page := `"limit":{"type":"integer","minimum":1,"maximum":200},"offset":{"type":"integer","minimum":0,"maximum":100000}`
	add(AnalyticsDashboard, "Read dashboard analytics attempts and activity within current project grants.", page, "", true, false)
	add(TimeSeries, "Read bounded bucketed project analytics with source and observation time.", page+`,"window":{"type":"string","maxLength":64},"bucket":{"type":"string","maxLength":64}`, "", true, false)
	add(Reports, "Read scoped usage, digest, efficiency and outcome reports with bounded population and freshness.", page+`,"row_offset":{"type":"integer","minimum":0,"maximum":100000}`+`,"from":{"type":"string","maxLength":64},"to":{"type":"string","maxLength":64},"bucket":{"type":"string","maxLength":64},"tz":{"type":"string","maxLength":128}`, "", true, false)
	add(InstanceHealth, "Read the existing instance health and readiness report.", "", "", true, false)
	add(NativeCapabilities, "Read the existing native protocol capabilities where the dashboard permits them.", "", "", true, false)
	add(OutboxHealth, "Read the existing outbox health report and bounded operator actions.", `"limit":{"type":"integer","minimum":1,"maximum":200},"cursor":{"type":"string","maxLength":2048}`, "", true, false)
	add(AIDebugPrompt, "Read the dashboard AI debug projection and prompt within current project grants.", `"scope":{"type":"string","enum":["fleet","project","issue"],"maxLength":256},"reference":`+boundedID, "", true, false)
	add(OperationsReport, "Read the dashboard operations report for a bounded time range.", `"since":{"type":"string","maxLength":64}`, "", true, false)
	add(RunnerFleet, "Read runner and host settings, health, capacity and optional project eligibility.", `"runner_id":`+boundedID+`,"limit":{"type":"integer","minimum":1,"maximum":200},"offset":{"type":"integer","minimum":0,"maximum":100000}`, "", true, false)
	add(Refresh, "Request the dashboard refresh; current refusals and coalescing still apply.", "", "", false, false)
	add(CapacityClear, "Request the existing capacity outage clear; requires operator approval.", `"scope":`+boundedID+`,"recovery":{"type":"string","maxLength":256,"enum":["ramping","immediate"]}`, "", false, true)
	add(TrackerAvailabilityClear, "Clear existing tracker availability conditions; requires operator approval.", "", "", false, true)
	add(ForgeAvailabilityClear, "Clear existing forge availability conditions; requires operator approval.", `"host":`+boundedID, "", false, true)
	add(FailureBreakerCanary, "Request the existing breaker canary; requires operator approval.", "", "", false, true)
	update := `{"type":"object","required":["expected_revision","expected_build_revision","service","version"],"properties":{"expected_revision":{"type":"integer","minimum":1},"expected_build_revision":{"type":"string","pattern":"^[a-f0-9]{64}$","maxLength":64},"service":{"type":"string","minLength":6,"maxLength":6,"enum":["detent"]},"version":` + boundedID + `,"release":{"type":"boolean"},"from_release":{"type":"boolean"}},"additionalProperties":false}`
	add(UpdateApply, "Apply through the installed updater and coordinated restart; Cloud requires runner_id and an observed change. Requires operator approval; acceptance is not running-build evidence.", `"release":{"type":"boolean"},"from_release":{"type":"boolean"},"runner_id":`+boundedID+`,"change":`+update, "", false, true)
	add(GetRunnerUpdate, "Read enrolled runner update support, requested/applied state and observed running build provenance.", `"runner_id":`+boundedID, `"runner_id"`, true, false)
	add(GetUrgentRunnerUpdate, "Read the organization's urgent runner release and current revision.", "", "", true, false)
	add(MarkUrgentRunnerUpdate, "Mark a release urgent for all older runners in this organization. Drains existing sessions before updating; requires operator approval.", `"change":{"type":"object","required":["expected_revision","version"],"properties":{"expected_revision":{"type":"integer","minimum":0},"version":`+boundedID+`},"additionalProperties":false}`, `"change"`, false, true)
	add(ProgressCredit, "Credit the exact issue through the dashboard command; requires operator approval.", `"reference":`+boundedID, `"project_id","reference"`, false, true)
	add(AcknowledgeWarnings, "Acknowledge active staleness warnings for this project.", `"warning_ids":`+boundedList, `"project_id","warning_ids"`, false, false)
	add(RecoverAttempt, "Perform an existing recovery action on an exact attempt; requires operator approval.", `"attempt_id":{"type":"integer","minimum":1},"action":{"type":"string","maxLength":256,"enum":["inspect","abandon","retry_fresh","retry_resume","cleanup_workspace"]},"reason":{"type":"string","maxLength":280}`, `"project_id","attempt_id","action"`, false, true)
	// Nested changes have the same fields and validation as the runner application.
	routing := `{"type":"object","required":["expected_revision","display_name","state","capacity_limit","project_ids"],"properties":{"expected_revision":{"type":"integer","minimum":1},"display_name":` + boundedID + `,"tags":{"type":"array","maxItems":100,"items":` + boundedID + `},"state":{"type":"string","maxLength":256,"enum":["active","draining","disabled"]},"capacity_limit":{"type":"integer","minimum":0,"maximum":10000},"project_ids":` + strings.Replace(boundedList, `"minItems":1`, `"minItems":0`, 1) + `,"home_project_ids":{"type":"array","maxItems":100,"items":` + boundedID + `},"isolation_tier":` + boundedID + `,"host_services":{"type":"array","maxItems":100,"items":` + boundedID + `},"availability":{"type":"object","properties":{"timezone":{"type":"string","maxLength":256},"windows":{"type":"array","maxItems":100,"items":` + boundedID + `},"hard_deadline":{"type":"string","maxLength":256}},"additionalProperties":false},"spillover":{"type":"object","properties":{"mode":{"type":"string","maxLength":256,"enum":["never","after"]},"after_minutes":{"type":"integer","minimum":0,"maximum":10080}},"additionalProperties":false}},"additionalProperties":false}`
	host := `{"type":"object","required":["expected_revision","display_name","capacity"],"properties":{"expected_revision":{"type":"integer","minimum":1},"display_name":` + boundedID + `,"capacity":{"type":"integer","minimum":0,"maximum":10000}},"additionalProperties":false}`
	for _, name := range []string{UpdateFleetRunner, UpdateRunnerRouting} {
		add(name, "Update exact runner settings and grants at the expected revision. Display edits run directly; material scheduling/access changes require operator approval.", `"runner_id":`+boundedID+`,"change":`+routing, `"runner_id","change"`, false, true)
	}
	for _, name := range []string{UpdateFleetHost, UpdateRunnerHost} {
		add(name, "Update exact host settings at the expected revision. Display edits run directly; capacity changes require operator approval.", `"machine_id":`+boundedID+`,"change":`+host, `"machine_id","change"`, false, true)
	}
	add(CreateRunnerEnrollment, "Create a bounded enrollment using current runner administration authority; requires operator approval. Token is returned only to the originating connection.", `"enrollment":{"type":"object","required":["project_ids","operations","ttl_seconds"],"properties":{"runner_id":{"type":"string","maxLength":256},"machine_id":{"type":"string","maxLength":256},"project_ids":`+boundedList+`,"operations":`+boundedList+`,"ttl_seconds":{"type":"integer","minimum":1,"maximum":900},"shared_machine":{"type":"boolean"}},"additionalProperties":false}`, `"enrollment"`, false, true)
	add(RevokeRunnerEnrollment, "Revoke an existing unused enrollment; requires operator approval.", `"enrollment_id":`+boundedID, `"enrollment_id"`, false, true)
	add(RevokeRunnerIdentity, "Remove the runner from fleet and routing, revoke its credential, and retain history. Refuses runners with active work; requires operator approval.", `"runner_id":`+boundedID, `"runner_id"`, false, true)
	capacity := `{"type":"object","required":["expected_revision","expected_config_revision","capacity"],"properties":{"expected_revision":{"type":"integer","minimum":1},"expected_config_revision":{"type":"string","minLength":64,"maxLength":64},"capacity":{"type":"integer","minimum":1,"maximum":10000},"backend":` + boundedID + `},"additionalProperties":false}`
	add(GetRunnerCapacity, "Read desired, applied and effective enrolled runner capacity with fresh binding-limit evidence.", `"runner_id":`+boundedID+`,"backend":`+boundedID, `"runner_id"`, true, false)
	add(UpdateRunnerCapacity, "Request capacity through the enrolled runner configuration owner. External provider producers remain authoritative; requires operator approval.", `"runner_id":`+boundedID+`,"change":`+capacity, `"runner_id","change"`, false, true)
	add(GetRunnerRouting, "Read exact runner settings with current administration authority.", `"runner_id":`+boundedID, `"runner_id"`, true, false)
	add(ListRunnerRouting, "Read runner settings with current administration authority.", `"limit":{"type":"integer","minimum":1,"maximum":200},"offset":{"type":"integer","minimum":0,"maximum":100000}`, "", true, false)
	add(HostedFleet, "Read the hosted fleet using current browser membership and project grants.", `"limit":{"type":"integer","minimum":1,"maximum":200},"offset":{"type":"integer","minimum":0,"maximum":100000}`, "", true, false)
	add(GitHubRequestCounts, "Read instance GitHub request counts with existing private administrator authority.", "", "", true, false)
	return definitions
}

func FleetDefinition(name string) (Definition, bool) {
	for _, d := range FleetCatalog() {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// ValidateFleetArguments enforces the advertised bounded shape before an
// adapter reaches application services. Business validation stays in commands.
func ValidateFleetArguments(name string, raw json.RawMessage) error {
	d, ok := FleetDefinition(name)
	if !ok || len(raw) > MaxArgumentBytes {
		return ErrInvalidArguments
	}
	var schema fleetSchema
	if json.Unmarshal(d.InputSchema, &schema) != nil {
		return ErrInvalidArguments
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&value) != nil || !schema.accepts(value) || decoder.Decode(new(any)) != io.EOF {
		return ErrInvalidArguments
	}
	return nil
}

// RoutingRequiresApproval permits display edits directly. Scheduling, access,
// isolation and capacity changes are material, even when they only add grants.
func RoutingRequiresApproval(before, after runnerauth.Routing) bool {
	before, after = before.Normalized(), after.Normalized()
	before.DisplayName, before.Tags = after.DisplayName, after.Tags
	before.CapacityRequest, after.CapacityRequest = nil, nil
	before.UpdateRequest, after.UpdateRequest = nil, nil
	return !reflect.DeepEqual(before, after)
}

type fleetSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]fleetSchema `json:"properties"`
	Required   []string               `json:"required"`
	Items      *fleetSchema           `json:"items"`
	Enum       []string               `json:"enum"`
	MinLength  int                    `json:"minLength"`
	MaxLength  int                    `json:"maxLength"`
	MinItems   int                    `json:"minItems"`
	MaxItems   int                    `json:"maxItems"`
	Minimum    *float64               `json:"minimum"`
	Maximum    *float64               `json:"maximum"`
}

func (s fleetSchema) accepts(value any) bool {
	switch s.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for _, required := range s.Required {
			if _, ok := object[required]; !ok {
				return false
			}
		}
		for key, v := range object {
			child, ok := s.Properties[key]
			if !ok || !child.accepts(v) {
				return false
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok || len(array) < s.MinItems || len(array) > s.MaxItems || s.Items == nil {
			return false
		}
		for _, v := range array {
			if !s.Items.accepts(v) {
				return false
			}
		}
	case "string":
		v, ok := value.(string)
		if !ok || len(v) < s.MinLength || len(v) > s.MaxLength || s.MinLength > 0 && strings.TrimSpace(v) == "" {
			return false
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, v) {
			return false
		}
	case "integer":
		v, ok := value.(float64)
		if !ok || math.Trunc(v) != v || s.Minimum != nil && v < *s.Minimum || s.Maximum != nil && v > *s.Maximum {
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	default:
		return false
	}
	return true
}
