package operatortool

import (
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

// ProjectRequest keeps business retry identity separate from protocol IDs and
// authority. Input is a concrete application input, never an operation proxy.
type ProjectRequest[T any] struct {
	ProjectID string `json:"project_id"`
	RequestID string `json:"request_id"`
	Input     T      `json:"input"`
}
type ProjectReadRequest struct {
	ProjectID        string `json:"project_id,omitempty"`
	ImportID         string `json:"import_id,omitempty"`
	RepositoryPolicy bool   `json:"repository_policy,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	After            string `json:"after,omitempty"`
}
type ProjectCreateInput struct {
	Name                string                `json:"name"`
	States              []tracker.NativeState `json:"states"`
	RequireDependencies *bool                 `json:"require_dependencies,omitempty"`
}
type HostedProjectCreateInput struct {
	States      *[]tracker.NativeState `json:"states,omitempty"`
	Name        string                 `json:"name"`
	GrantAccess bool                   `json:"grant_access"`
}
type OnboardingInput struct {
	Progress onboarding.Progress `json:"progress"`
}
type SpriteUsageInput struct {
	Observations []usagecost.Observation `json:"observations"`
}
type IntegrationInput struct {
	WorkflowMarkdown  *string                `json:"workflow_markdown,omitempty"`
	States            *[]tracker.NativeState `json:"states,omitempty"`
	ExpectedRevision  tracker.Revision       `json:"expected_revision,string"`
	Intake            string                 `json:"intake"`
	Projection        string                 `json:"projection"`
	RepositoryEnabled bool                   `json:"repository_enabled"`
}
type RepositoryInput struct {
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
	Repository       string           `json:"repository"`
	Source           string           `json:"source,omitempty"`
}
type ImportStartInput struct {
	IssueNumber      int              `json:"issue_number"`
	Restart          bool             `json:"restart"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}
type ImportAdvanceInput struct {
	ImportID         string           `json:"import_id"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}
type GitHubBatchInput struct {
	Revision      int64    `json:"revision"`
	Action        string   `json:"action"`
	RunnerID      string   `json:"runner_id"`
	Labels        []string `json:"labels,omitempty"`
	IncludeClosed bool     `json:"include_closed"`
	Numbers       []int    `json:"numbers,omitempty"`
	Destination   string   `json:"destination"`
	AllowDispatch bool     `json:"allow_dispatch"`
}
type CutoverInput struct {
	ClosedState    string                `json:"closed_state"`
	DryRun         bool                  `json:"dry_run"`
	Checkpoint     string                `json:"checkpoint"`
	AcceptPartial  bool                  `json:"accept_partial"`
	CloseSource    bool                  `json:"close_source"`
	DestinationURL string                `json:"destination_url"`
	States         []tracker.NativeState `json:"states"`
	InitialState   string                `json:"initial_state"`
}

type PolicyApprovalInput struct {
	ExpectedID       string            `json:"expected_policy_id"`
	Policy           policy.Descriptor `json:"policy"`
	RepositoryPolicy bool              `json:"repository_policy,omitempty"`
}

type PolicyRevokeInput struct {
	RepositoryPolicy bool   `json:"repository_policy,omitempty"`
	ExpectedID       string `json:"expected_policy_id"`
}
type SummaryInput struct {
	ItemID string `json:"item_id"`
	Body   string `json:"body"`
}

// ProjectCatalog includes only named, typed operations. Browser setup tools
// return navigation and requirements without accepting credentials or file paths.
func ProjectCatalog() []Definition {
	reads := []string{"list_projects", "get_native_project", "get_onboarding", "get_project_integration", "get_cutover_receipt", "get_git_hub_import", "list_git_hub_import_records", "get_git_hub_batch", "get_project_policy", "project_secret_metadata", "repository_freshness", "project_settings", "project_setup", "demo_setup_scenarios"}
	out := make([]Definition, 0, len(reads)+14)
	for _, name := range reads {
		schema := projectSchema(reflect.TypeFor[ProjectReadRequest]())
		required := []string{"project_id"}
		if name == "list_projects" || name == "project_setup" || name == "demo_setup_scenarios" {
			required = []string{}
		}
		if name == "get_git_hub_import" || name == "list_git_hub_import_records" {
			required = append(required, "import_id")
		}
		schema["required"] = required
		raw, _ := json.Marshal(schema) //nolint:errcheck // Generated schemas contain only JSON primitives, slices and maps.
		out = append(out, Definition{Name: name, Description: "Read authorized project application data: " + strings.ReplaceAll(name, "_", " ") + ". Setup returns the existing browser flow and requirements.", InputSchema: raw, Annotations: Annotations{ReadOnly: true, Idempotent: true}, Meta: ToolMetadata{Toolset: "projects"}})
	}
	out = append(out,
		Definition{Name: "monthly_usage_costs", Description: "Read UTC monthly infrastructure cost totals and coverage. Project totals are a breakdown of organization totals, never an additional charge.", InputSchema: json.RawMessage(`{"type":"object","properties":{"project_id":{"type":"string","maxLength":256},"month":{"type":"string","pattern":"^[0-9]{4}-[0-9]{2}$"},"scope":{"type":"string","enum":["project","organization"]}},"additionalProperties":false}`), Annotations: Annotations{ReadOnly: true, Idempotent: true}, Meta: ToolMetadata{Toolset: "projects"}},
		projectWrite[ProjectCreateInput]("create_native_project", false, false),
		projectWrite[HostedProjectCreateInput]("create_hosted_project", true, false),
		projectWrite[OnboardingInput]("save_onboarding", false, false),
		projectWrite[IntegrationInput]("update_project_integration", true, true),
		projectWrite[RepositoryInput]("bind_native_repository", true, true),
		projectWrite[ImportStartInput]("start_git_hub_import", true, true),
		projectWrite[ImportAdvanceInput]("advance_git_hub_import", false, true),
		projectWrite[GitHubBatchInput]("command_git_hub_batch", true, true),
		projectWrite[CutoverInput]("cutover_project", true, true),
		projectWrite[PolicyApprovalInput]("approve_project_policy", true, false),
		projectWrite[PolicyRevokeInput]("revoke_project_policy", true, false),
		projectWrite[SummaryInput]("project_native_summary", true, true),
		projectWrite[struct{}]("remove_project_secret", true, false),
		projectWrite[SpriteUsageInput]("import_sprite_usage", false, false),
	)
	return out
}
func projectWrite[T any](name string, destructive, openWorld bool) Definition {
	schema := projectSchema(reflect.TypeFor[ProjectRequest[T]]())
	schema["required"] = []string{"project_id", "request_id", "input"}
	if name == "import_sprite_usage" {
		input := schema["properties"].(map[string]any)["input"].(map[string]any)
		observations := input["properties"].(map[string]any)["observations"].(map[string]any)
		observations["minItems"], observations["maxItems"] = 1, 128
		properties := observations["items"].(map[string]any)["properties"].(map[string]any)
		properties["quantity"] = map[string]any{"type": []string{"number", "null"}, "minimum": 0}
		properties["amount_micros"] = map[string]any{"type": []string{"integer", "null"}, "minimum": -1e15, "maximum": 1e15}
		properties["unit_price_micros"] = map[string]any{"type": []string{"integer", "null"}, "minimum": 0, "maximum": 1e15}
		for _, field := range []string{"rate_source", "evidence_source", "resource_name"} {
			properties[field] = map[string]any{"type": "string", "maxLength": 512}
		}
	}
	if name == "create_native_project" || name == "create_hosted_project" {
		schema["required"] = []string{"request_id", "input"}
	}
	raw, _ := json.Marshal(schema) //nolint:errcheck // Generated schemas contain only JSON primitives, slices and maps.
	return Definition{Name: name, Description: "Use the shared dashboard application command: " + strings.ReplaceAll(name, "_", " ") + ". Current scoped authority is required; request_id is the business retry key.", InputSchema: raw, Annotations: Annotations{Destructive: destructive, Idempotent: true, OpenWorld: openWorld}, Meta: ToolMetadata{Toolset: "projects"}}
}

func projectSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}
	}
	if t == reflect.TypeFor[time.Time]() {
		return map[string]any{"type": "string", "format": "date-time", "maxLength": 64}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := range t.NumField() {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" || tag[0] == "" {
				continue
			}
			child := projectSchema(f.Type)
			if len(tag) > 1 && tag[1] == "string" {
				child = map[string]any{"type": "string", "maxLength": 20, "pattern": "^[0-9]+$"}
			}
			if f.Type.Kind() == reflect.String {
				child["maxLength"] = projectStringLimit(tag[0])
			}
			if tag[0] == "limit" {
				child["minimum"], child["maximum"] = 1, 200
			}
			props[tag[0]] = child
			if !strings.Contains(f.Tag.Get("json"), ",omitempty") {
				required = append(required, tag[0])
			}
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	case reflect.Slice:
		return map[string]any{"type": "array", "maxItems": 200, "items": projectSchema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer", "minimum": 0, "maximum": 2147483647}
	case reflect.Float64:
		return map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 1e9}
	default:
		return map[string]any{"type": "string", "maxLength": 256}
	}
}

// DecodeProjectArguments applies bounds before application services or approval
// are reached, including recursive fields that JSON schema cannot enforce alone.
func DecodeProjectArguments(raw json.RawMessage, target any) error {
	if err := DecodeArguments(raw, target); err != nil {
		return ErrInvalidArguments
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ErrInvalidArguments
	}
	var bounded func(any, string, reflect.Type) bool
	bounded = func(v any, key string, t reflect.Type) bool {
		if t.Kind() == reflect.Pointer {
			if v == nil && (key == "quantity" || key == "amount_micros" || key == "unit_price_micros") {
				return true
			}
			t = t.Elem()
		}
		if t == reflect.TypeFor[json.RawMessage]() {
			return true
		}
		switch x := v.(type) {
		case string:
			maximum := projectStringLimit(key)
			return len(x) <= maximum
		case []any:
			if len(x) > 200 {
				return false
			}
			for _, item := range x {
				if !bounded(item, key, t.Elem()) {
					return false
				}
			}
		case map[string]any:
			for i := range t.NumField() {
				field := t.Field(i)
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if item, present := x[name]; present && !bounded(item, name, field.Type) {
					return false
				}
			}
		case float64:
			if key == "amount_micros" {
				return x >= -1e15 && x <= 1e15
			}
			if key == "unit_price_micros" || key == "quantity" {
				return x >= 0 && x <= 1e15
			}
			if key == "limit" {
				return x >= 1 && x <= 200
			}
			return x >= 0 && x <= 2147483647
		case nil:
			return false
		}
		return true
	}
	if !bounded(value, "", reflect.TypeOf(target)) {
		return ErrInvalidArguments
	}
	return nil
}

func projectStringLimit(key string) int {
	switch key {
	case "rate_source", "evidence_source", "resource_name":
		return 512
	case "prompt", "shared_prompt", "agents_prompt":
		return MaxArgumentBytes
	case "source":
		return 1024
	case "request_id":
		return 128
	case "body":
		return 32768
	case "reason":
		return 1120
	case "destination_url":
		return 2048
	}
	return 256
}
