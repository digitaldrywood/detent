package hubserver

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestOperatorEnvelopeDecoderContract(t *testing.T) {
	for _, test := range []struct {
		name, field string
		target      reflect.Type
	}{
		{"create_workspace", "input", reflect.TypeFor[workspaceRequest]()},
		{"create_conversation", "input", reflect.TypeFor[conversationCreateRequest]()},
		{"post_conversation_command", "input", reflect.TypeFor[conversation.Command]()},
		{"link_conversation", "input", reflect.TypeFor[conversationLinkRequest]()},
		{"patch_conversation", "input", reflect.TypeFor[conversationPatchRequest]()},
		{"upload_conversation_attachment", "input", reflect.TypeFor[operatortool.ConversationAttachmentArguments]()},
		{"create_project_action", "input", reflect.TypeFor[projectActionRequest]()},
		{"patch_project_action", "input", reflect.TypeFor[projectActionPatch]()},
		{operatortool.MarkUrgentRunnerUpdate, "change", reflect.TypeFor[urgentRunnerUpdateChange]()},
		{operatortool.UpdateApply, "change", reflect.TypeFor[runnerUpdateChange]()},
		{operatortool.UpdateRunnerCapacity, "change", reflect.TypeFor[runnerCapacityChange]()},
		{operatortool.UpdateRunnerRouting, "change", reflect.TypeFor[runnerRoutingRequest]()},
		{operatortool.UpdateRunnerHost, "change", reflect.TypeFor[runnerauth.HostChange]()},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition, found := operatortool.Lookup(test.name)
			if !found {
				t.Fatalf("%s absent from registry", test.name)
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			raw, err := envelopeContractValue(schema.Properties[test.field])
			if err != nil {
				t.Fatal(err)
			}
			value, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := operatortool.DecodeArguments(value, reflect.New(test.target).Interface()); err != nil {
				t.Fatalf("%s.%s: %v; advertised=%s; sample=%s", test.name, test.field, err, schema.Properties[test.field], value)
			}
		})
	}
}

func envelopeContractValue(raw json.RawMessage) (any, error) {
	var schema struct {
		Type       any                        `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Items      json.RawMessage            `json:"items"`
		Enum       []any                      `json:"enum"`
		Minimum    float64                    `json:"minimum"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if len(schema.Enum) > 0 {
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
	switch kind {
	case "object", "":
		fields := map[string]any{}
		for name, child := range schema.Properties {
			value, err := envelopeContractValue(child)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			fields[name] = value
		}
		return fields, nil
	case "string":
		return "1", nil
	case "integer", "number":
		return schema.Minimum, nil
	case "boolean":
		return false, nil
	case "array":
		value, err := envelopeContractValue(schema.Items)
		return []any{value}, err
	default:
		return nil, fmt.Errorf("unsupported schema type %v", schema.Type)
	}
}
