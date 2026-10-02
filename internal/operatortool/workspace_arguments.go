package operatortool

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// ValidateWorkspaceArguments enforces the bounded catalog even for clients that
// bypass discovery. Application-specific validation still runs in the command.
func ValidateWorkspaceArguments(call Call) error {
	for _, definition := range append(WorkspaceCatalog(), OperatorChatCatalog()...) {
		if definition.Name != call.Name {
			continue
		}
		var schema argumentSchema
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			return err
		}
		var value any
		if err := DecodeArguments(call.Arguments, &value); err != nil {
			return err
		}
		if !schema.accepts(value) {
			return ErrInvalidArguments
		}
		return nil
	}
	return ErrUnknownTool
}

type argumentSchema struct {
	Type          string                    `json:"type"`
	Properties    map[string]argumentSchema `json:"properties"`
	Required      []string                  `json:"required"`
	Additional    json.RawMessage           `json:"additionalProperties"`
	Items         *argumentSchema           `json:"items"`
	Enum          []string                  `json:"enum"`
	MinLength     int                       `json:"minLength"`
	MaxLength     int                       `json:"maxLength"`
	MaxItems      int                       `json:"maxItems"`
	MaxProperties int                       `json:"maxProperties"`
	Minimum       *float64                  `json:"minimum"`
	Maximum       *float64                  `json:"maximum"`
}

func (s argumentSchema) accepts(value any) bool {
	switch s.Type {
	case "object":
		fields, ok := value.(map[string]any)
		if !ok || s.MaxProperties > 0 && len(fields) > s.MaxProperties {
			return false
		}
		for _, key := range s.Required {
			if _, ok := fields[key]; !ok {
				return false
			}
		}
		for key, value := range fields {
			child, found := s.Properties[key]
			if !found {
				if string(s.Additional) == "false" || len(s.Additional) == 0 {
					return false
				}
				if json.Unmarshal(s.Additional, &child) != nil {
					return false
				}
			}
			if !child.accepts(value) {
				return false
			}
		}
	case "string":
		v, ok := value.(string)
		if !ok || utf8.RuneCountInString(v) < s.MinLength || s.MaxLength > 0 && len(v) > s.MaxLength {
			return false
		}
		if len(s.Enum) > 0 {
			for _, allowed := range s.Enum {
				if v == allowed {
					return true
				}
			}
			return false
		}
	case "array":
		v, ok := value.([]any)
		if !ok || s.MaxItems > 0 && len(v) > s.MaxItems || s.Items == nil {
			return false
		}
		for _, item := range v {
			if !s.Items.accepts(item) {
				return false
			}
		}
	case "integer":
		v, ok := value.(float64)
		if !ok || v != float64(int64(v)) || s.Minimum != nil && v < *s.Minimum || s.Maximum != nil && v > *s.Maximum {
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

// WorkspaceResult preserves the application's typed projection and rejects
// oversized output before it reaches either transport.
func WorkspaceResult(value any) (Result, error) { return encodeResult(value) }

func WorkspaceDefinition(name string) (Definition, error) {
	for _, definition := range WorkspaceCatalog() {
		if definition.Name == name {
			return definition, nil
		}
	}
	return Definition{}, fmt.Errorf("%w", ErrUnknownTool)
}
