package operatortool

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"time"
)

type contractSchema struct {
	Type                 any                       `json:"type"`
	Properties           map[string]contractSchema `json:"properties"`
	Required             []string                  `json:"required"`
	Enum                 []any                     `json:"enum"`
	Format               string                    `json:"format"`
	ContentEncoding      string                    `json:"contentEncoding"`
	Ref                  string                    `json:"$ref"`
	Defs                 map[string]contractSchema `json:"$defs"`
	ExclusiveMinimum     *float64                  `json:"exclusiveMinimum"`
	Pattern              string                    `json:"pattern"`
	Minimum              float64                   `json:"minimum"`
	Maximum              *float64                  `json:"maximum"`
	MinLength            int                       `json:"minLength"`
	MaxLength            *int                      `json:"maxLength"`
	MinItems             int                       `json:"minItems"`
	MaxItems             *int                      `json:"maxItems"`
	AnyOf                []contractSchema          `json:"anyOf"`
	OneOf                []contractSchema          `json:"oneOf"`
	Items                *contractSchema           `json:"items"`
	AdditionalProperties json.RawMessage           `json:"additionalProperties"`
}

func TestCatalogDecoderContract(t *testing.T) {
	targets := map[string]reflect.Type{
		BoardState:       reflect.TypeFor[boardStateArguments](),
		FleetHealth:      reflect.TypeFor[struct{}](),
		TelemetryUsage:   reflect.TypeFor[telemetryUsageArguments](),
		RecentActivity:   reflect.TypeFor[recentActivityArguments](),
		ExplainItem:      reflect.TypeFor[explainItemArguments](),
		MoveItem:         reflect.TypeFor[MoveItemArguments](),
		SetPriority:      reflect.TypeFor[SetPriorityArguments](),
		FileIssue:        reflect.TypeFor[FileIssueArguments](),
		UploadAttachment: reflect.TypeFor[AttachmentArguments](),
	}
	definitions := Registry()
	for name, target := range map[string]reflect.Type{
		StopRun:               reflect.TypeFor[StopRunArguments](),
		ActionResult:          reflect.TypeFor[ActionResultArguments](),
		ConnectionInfo:        reflect.TypeFor[struct{}](),
		AppBootstrapPayload:   reflect.TypeFor[struct{}](),
		AppUpdates:            reflect.TypeFor[struct{}](),
		HostedEvents:          reflect.TypeFor[HostedEventArguments](),
		"get_operator_chat":   reflect.TypeFor[OperatorChatReadArguments](),
		"post_operator_chat":  reflect.TypeFor[OperatorChatArguments](),
		"monthly_usage_costs": reflect.TypeFor[MonthlyUsageArguments](),
		BillingStatus:         reflect.TypeFor[struct{}](),
		BillingUsage:          reflect.TypeFor[struct{}](),
		BillingExport:         reflect.TypeFor[struct{}](),
		HostedPlan:            reflect.TypeFor[struct{}](),
		HostedUsage:           reflect.TypeFor[HostedUsageArguments](),
		BillingCheckout:       reflect.TypeFor[CheckoutArguments](),
		CreditCheckout:        reflect.TypeFor[CheckoutArguments](),
		CreditAutoFund:        reflect.TypeFor[AutoFundArguments](),
		BillingPortal:         reflect.TypeFor[PortalArguments](),
		BudgetOverrideSet:     reflect.TypeFor[BudgetArguments](),
		BudgetOverrideClear:   reflect.TypeFor[BudgetArguments](),
		UsageReport:           reflect.TypeFor[UsageArguments](),
		IssueExplanation:      reflect.TypeFor[explainItemArguments](),
	} {
		targets[name] = target
	}
	for _, definition := range ProjectCatalog() {
		if definition.Annotations.ReadOnly && targets[definition.Name] == nil {
			targets[definition.Name] = reflect.TypeFor[ProjectReadRequest]()
		}
	}
	for name, target := range map[string]reflect.Type{
		"create_native_project":          reflect.TypeFor[ProjectRequest[ProjectCreateInput]](),
		"create_hosted_project":          reflect.TypeFor[ProjectRequest[HostedProjectCreateInput]](),
		"save_onboarding":                reflect.TypeFor[ProjectRequest[OnboardingInput]](),
		"update_project_integration":     reflect.TypeFor[ProjectRequest[IntegrationInput]](),
		"bind_native_repository":         reflect.TypeFor[ProjectRequest[RepositoryInput]](),
		"start_git_hub_import":           reflect.TypeFor[ProjectRequest[ImportStartInput]](),
		"advance_git_hub_import":         reflect.TypeFor[ProjectRequest[ImportAdvanceInput]](),
		"command_git_hub_batch":          reflect.TypeFor[ProjectRequest[GitHubBatchInput]](),
		"cutover_project":                reflect.TypeFor[ProjectRequest[CutoverInput]](),
		"approve_project_policy":         reflect.TypeFor[ProjectRequest[PolicyApprovalInput]](),
		"revoke_project_policy":          reflect.TypeFor[ProjectRequest[PolicyRevokeInput]](),
		"project_native_summary":         reflect.TypeFor[ProjectRequest[SummaryInput]](),
		"remove_project_secret":          reflect.TypeFor[ProjectRequest[struct{}]](),
		"import_sprite_usage":            reflect.TypeFor[ProjectRequest[SpriteUsageInput]](),
		GetOrganizationModelSelection:    reflect.TypeFor[ModelSelectionReadRequest](),
		GetProjectModelSelection:         reflect.TypeFor[ModelSelectionReadRequest](),
		UpdateOrganizationModelSelection: reflect.TypeFor[ProjectRequest[ModelSelectionInput]](),
		UpdateProjectModelSelection:      reflect.TypeFor[ProjectRequest[ModelSelectionInput]](),
	} {
		targets[name] = target
	}
	for _, group := range []struct {
		catalog []Definition
		target  reflect.Type
	}{
		{LocalProjectCatalog(), reflect.TypeFor[LocalProjectArguments]()},
		{WorkspaceCatalog(), reflect.TypeFor[WorkspaceArguments]()},
		{ChangeCatalog(), reflect.TypeFor[ChangeArguments]()},
		{AdministrationCatalog(), reflect.TypeFor[AdministrationArguments]()},
		{FleetCatalog(), reflect.TypeFor[FleetArguments]()},
		{PlatformCreditCatalog(), reflect.TypeFor[PlatformCreditArguments]()},
	} {
		for _, definition := range group.catalog {
			targets[definition.Name] = group.target
		}
	}
	targets[Activity] = reflect.TypeFor[ActivityRequest]()
	for _, name := range []string{AnalyticsDashboard, TimeSeries, Reports} {
		targets[name] = reflect.TypeFor[AnalyticsRequest]()
	}
	for _, name := range []string{InstanceHealth, NativeCapabilities, OutboxHealth, GetRunnerUpdate, GetUrgentRunnerUpdate, MarkUrgentRunnerUpdate, GetRunnerRouting, ListRunnerRouting, GetRunnerCapacity, UpdateRunnerCapacity, UpdateRunnerRouting, UpdateRunnerHost, UpdateApply, HostedFleet, GitHubRequestCounts, CreateRunnerEnrollment, RevokeRunnerEnrollment, RevokeRunnerIdentity} {
		targets[name] = reflect.TypeFor[HubFleetArguments]()
	}
	for _, definition := range definitions {
		switch {
		case IsWorkRead(definition.Name):
			targets[definition.Name] = reflect.TypeFor[WorkReadRequest]()
		case IsWorkTool(definition.Name):
			targets[definition.Name] = reflect.TypeFor[WorkArguments]()
		case IsAttachmentTool(definition.Name) && definition.Name != UploadAttachment:
			targets[definition.Name] = reflect.TypeFor[AttachmentOperationArguments]()
		}
	}
	propertiesByTarget := map[reflect.Type]map[string]contractSchema{}
	namesByTarget := map[reflect.Type][]string{}
	for _, definition := range definitions {
		decoderTargets := []reflect.Type{targets[definition.Name]}
		if definition.Name == UpdateApply {
			decoderTargets = append(decoderTargets, reflect.TypeFor[FleetArguments]())
		}
		if definition.Name == Dashboard {
			decoderTargets = append(decoderTargets, reflect.TypeFor[boardStateArguments]())
		}
		for _, target := range decoderTargets {
			t.Run(definition.Name, func(t *testing.T) {
				if target == nil {
					t.Fatalf("%s: no decoder target registered", definition.Name)
				}
				namesByTarget[target] = append(namesByTarget[target], definition.Name)
				var schema contractSchema
				if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
					t.Fatalf("%s: schema: %v", definition.Name, err)
				}
				if propertiesByTarget[target] == nil {
					propertiesByTarget[target] = map[string]contractSchema{}
				}
				for field, property := range schema.Properties {
					if field != "request_id" || contractHasField(target, "request_id") {
						propertiesByTarget[target][field] = property
					}
				}
				schema = contractResolve(schema, schema.Defs, 0)
				if slices.Contains([]string{ArchiveItem, OrderItem, SetQueuePriority}, definition.Name) {
					t.Log("expected hosted availability gap: #615 owns this tool")
				}
				for _, all := range []bool{false, true} {
					value, err := contractValue(schema, all)
					if err != nil {
						t.Fatalf("%s: sample: %v", definition.Name, err)
					}
					fields := value.(map[string]any)
					if _, accepted := propertiesByTarget[target]["request_id"]; !accepted {
						delete(fields, "request_id")
					}
					raw, err := json.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					if err := decodeArguments(raw, reflect.New(target).Interface()); err != nil {
						var detail *RequestError
						if errors.As(err, &detail) {
							t.Fatalf("%s all_optional=%t: %s; arguments=%s", definition.Name, all, detail.Message, raw)
						}
						t.Fatalf("%s all_optional=%t: %v; arguments=%s", definition.Name, all, err, raw)
					}
				}
			})
		}
	}

	for target, properties := range propertiesByTarget {
		t.Run(target.String()+"/documented_fields", func(t *testing.T) {
			contractFields(t, target, contractSchema{Type: "object", Properties: properties}, strings.Join(namesByTarget[target], ","))
		})
	}
	for _, seed := range []struct {
		name   string
		target any
	}{
		{EditItem, &WorkArguments{}},
		{MoveItem, &MoveItemArguments{}},
	} {
		t.Run(seed.name+"/revision_seed", func(t *testing.T) {
			definition, _ := Lookup(seed.name)
			var schema contractSchema
			if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			value, err := contractValue(schema.Properties["expected_revision"], false)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"expected_revision": value})
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeArguments(raw, seed.target); err != nil {
				t.Fatalf("%s advertised revision %s: %v", seed.name, raw, err)
			}
			var detail *RequestError
			err = decodeArguments(json.RawMessage(`{"expected_revision":1}`), seed.target)
			if !errors.As(err, &detail) || detail.Message != "expected_revision: must be a string" {
				t.Fatalf("%s: integer revision error=%v detail=%#v", seed.name, err, detail)
			}
		})
	}
}

func contractFields(t *testing.T, target reflect.Type, schema contractSchema, path string) {
	t.Helper()
	for target.Kind() == reflect.Pointer || target.Kind() == reflect.Slice {
		target = target.Elem()
		if schema.Items != nil {
			schema = *schema.Items
		}
	}
	if target.Kind() != reflect.Struct || schema.Ref != "" || target == reflect.TypeFor[time.Time]() {
		return
	}
	for i := range target.NumField() {
		field := target.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			contractFields(t, field.Type, schema, path)
			continue
		}
		if name == "" {
			name = field.Name
		}
		property, found := schema.Properties[name]
		if !found {
			t.Errorf("%s.%s: decoder field absent from catalog properties", path, name)
			continue
		}
		contractFields(t, field.Type, property, path+"."+name)
	}
}

func contractValue(schema contractSchema, all bool) (any, error) {
	if len(schema.Enum) != 0 {
		return schema.Enum[0], nil
	}
	kind, _ := schema.Type.(string)
	if types, ok := schema.Type.([]any); ok {
		for _, value := range types {
			if value != "null" {
				kind, _ = value.(string)
				break
			}
		}
	}
	if schema.Ref != "" {
		return map[string]any{}, nil
	}
	switch kind {
	case "", "object":
		for _, alternatives := range [][]contractSchema{schema.AnyOf, schema.OneOf} {
			if len(alternatives) != 0 {
				schema.Required = append(slices.Clone(schema.Required), alternatives[0].Required...)
			}
		}
		fields := map[string]any{}
		for name, child := range schema.Properties {
			if !all && !slices.Contains(schema.Required, name) {
				continue
			}
			value, err := contractValue(child, all)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			fields[name] = value
		}
		for _, name := range schema.Required {
			if _, found := fields[name]; !found {
				return nil, fmt.Errorf("%s: required property absent from schema", name)
			}
		}
		if all && len(schema.Properties) == 0 && len(schema.AdditionalProperties) > 0 && string(schema.AdditionalProperties) != "false" && string(schema.AdditionalProperties) != "true" {
			var child contractSchema
			if err := json.Unmarshal(schema.AdditionalProperties, &child); err != nil {
				return nil, err
			}
			value, err := contractValue(child, all)
			if err != nil {
				return nil, err
			}
			fields["sample"] = value
		}
		return fields, nil
	case "string":
		value := strings.Repeat("x", max(1, schema.MinLength))
		if schema.Format == "date-time" {
			value = "2026-01-01T00:00:00Z"
		}
		if schema.ContentEncoding == "base64" {
			value = base64.StdEncoding.EncodeToString([]byte(value))
		}
		if schema.Pattern != "" {
			pattern, err := syntax.Parse(schema.Pattern, syntax.Perl)
			if err != nil {
				return nil, err
			}
			value, err = contractPattern(pattern)
			if err != nil {
				return nil, err
			}
			if matched, err := regexp.MatchString(schema.Pattern, value); err != nil || !matched || len(value) < schema.MinLength {
				return nil, fmt.Errorf("generated %q does not satisfy pattern %q and minLength %d", value, schema.Pattern, schema.MinLength)
			}
		}
		if schema.MaxLength != nil && len(value) > *schema.MaxLength {
			return nil, errors.New("string exceeds maxLength")
		}
		return value, nil
	case "integer":
		value := math.Ceil(schema.Minimum)
		if schema.Maximum != nil && value > *schema.Maximum {
			return nil, errors.New("integer exceeds maximum")
		}
		return int64(value), nil
	case "number":
		value := schema.Minimum
		if schema.ExclusiveMinimum != nil {
			value = *schema.ExclusiveMinimum + 1
		}
		return value, nil
	case "boolean":
		return false, nil
	case "array":
		if schema.Items == nil {
			return nil, errors.New("array has no items schema")
		}
		size := schema.MinItems
		if all && size == 0 {
			size = 1
		}
		if schema.MaxItems != nil && size > *schema.MaxItems {
			return nil, errors.New("array exceeds maxItems")
		}
		values := make([]any, size)
		for i := range values {
			value, err := contractValue(*schema.Items, all)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		return values, nil
	default:
		return nil, fmt.Errorf("unsupported type %q", schema.Type)
	}
}

func contractPattern(pattern *syntax.Regexp) (string, error) {
	switch pattern.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginText, syntax.OpEndText, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpWordBoundary:
		return "", nil
	case syntax.OpLiteral:
		return string(pattern.Rune), nil
	case syntax.OpCharClass:
		return string(pattern.Rune[0]), nil
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return "x", nil
	case syntax.OpCapture, syntax.OpAlternate, syntax.OpPlus, syntax.OpRepeat:
		value, err := contractPattern(pattern.Sub[0])
		if pattern.Op == syntax.OpRepeat {
			value = strings.Repeat(value, pattern.Min)
		}
		return value, err
	case syntax.OpStar, syntax.OpQuest:
		return "", nil
	case syntax.OpConcat:
		var result strings.Builder
		for _, child := range pattern.Sub {
			value, err := contractPattern(child)
			if err != nil {
				return "", err
			}
			result.WriteString(value)
		}
		return result.String(), nil
	default:
		return "", fmt.Errorf("unsupported pattern operation %v", pattern.Op)
	}
}

func contractHasField(target reflect.Type, name string) bool {
	for i := range target.NumField() {
		if strings.Split(target.Field(i).Tag.Get("json"), ",")[0] == name {
			return true
		}
	}
	return false
}

func contractResolve(schema contractSchema, definitions map[string]contractSchema, depth int) contractSchema {
	if schema.Ref != "" {
		if depth > 8 {
			return schema
		}
		name := strings.TrimPrefix(schema.Ref, "#/$defs/")
		if value, found := definitions[name]; found {
			schema = value
		}
	}
	properties := make(map[string]contractSchema, len(schema.Properties))
	for name, child := range schema.Properties {
		properties[name] = contractResolve(child, definitions, depth+1)
	}
	schema.Properties = properties
	if schema.Items != nil {
		value := contractResolve(*schema.Items, definitions, depth+1)
		schema.Items = &value
	}
	return schema
}
