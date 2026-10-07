package operatortool

import (
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
)

type contractSchema struct {
	Type                 string                    `json:"type"`
	Properties           map[string]contractSchema `json:"properties"`
	Required             []string                  `json:"required"`
	Enum                 []any                     `json:"enum"`
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
	definitions := append(Catalog(), WorkReadCatalog()...)
	definitions = append(definitions, WorkCatalog()...)
	definitions = append(definitions, AttachmentCatalog()...)
	for _, name := range []string{MoveItem, SetPriority, FileIssue} {
		definition, found := Lookup(name)
		if !found {
			t.Fatalf("%s: missing definition", name)
		}
		definitions = append(definitions, definition)
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
		t.Run(definition.Name, func(t *testing.T) {
			target, found := targets[definition.Name]
			if !found {
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
				if field != "request_id" || target == reflect.TypeFor[MoveItemArguments]() || target == reflect.TypeFor[AttachmentArguments]() || target == reflect.TypeFor[AttachmentOperationArguments]() {
					propertiesByTarget[target][field] = property
				}
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
	if target.Kind() != reflect.Struct {
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
	switch schema.Type {
	case "object":
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
		return fields, nil
	case "string":
		value := strings.Repeat("x", max(1, schema.MinLength))
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
		return schema.Minimum, nil
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
