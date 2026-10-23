package operatortool

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
		return schema.validate(value, "arguments")
	}
	return ErrUnknownTool
}

type argumentSchema struct {
	Type          string                    `json:"type"`
	Pattern       string                    `json:"pattern"`
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

func invalidArgument(field, rule string) error {
	return &RequestError{Code: "invalid_request", Message: field + ": " + rule}
}

func validateArgumentSchema(raw json.RawMessage, schema argumentSchema) error {
	var value any
	if err := DecodeArguments(raw, &value); err != nil {
		return err
	}
	return schema.validate(value, "arguments")
}

func (s argumentSchema) accepts(value any) bool {
	return s.validate(value, "arguments") == nil
}

func (s argumentSchema) validate(value any, field string) error {
	switch s.Type {
	case "object":
		fields, ok := value.(map[string]any)
		if !ok {
			return invalidArgument(field, "must be an object")
		}
		if s.MaxProperties > 0 && len(fields) > s.MaxProperties {
			return invalidArgument(field, fmt.Sprintf("must contain at most %d properties", s.MaxProperties))
		}
		for _, key := range s.Required {
			if _, ok := fields[key]; !ok {
				return invalidArgument(argumentField(field, key), "is required")
			}
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			child, found := s.Properties[key]
			if !found {
				if string(s.Additional) == "false" || len(s.Additional) == 0 || json.Unmarshal(s.Additional, &child) != nil {
					return invalidArgument(argumentField(field, key), "is not an allowed field")
				}
			}
			if err := child.validate(fields[key], argumentField(field, key)); err != nil {
				return err
			}
		}
	case "string":
		v, ok := value.(string)
		if !ok {
			return invalidArgument(field, "must be a string")
		}
		if utf8.RuneCountInString(v) < s.MinLength {
			return invalidArgument(field, fmt.Sprintf("must contain at least %d characters", s.MinLength))
		}
		if s.MaxLength > 0 && len(v) > s.MaxLength {
			return invalidArgument(field, fmt.Sprintf("must not exceed %d bytes", s.MaxLength))
		}
		if s.Pattern != "" {
			matched, err := regexp.MatchString(s.Pattern, v)
			if err != nil {
				return err
			}
			if !matched {
				return invalidArgument(field, "must match pattern "+s.Pattern)
			}
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, v) {
			return invalidArgument(field, "must be one of: "+strings.Join(s.Enum, ", "))
		}
	case "array":
		v, ok := value.([]any)
		if !ok {
			return invalidArgument(field, "must be an array")
		}
		if s.MaxItems > 0 && len(v) > s.MaxItems {
			return invalidArgument(field, fmt.Sprintf("must contain at most %d items", s.MaxItems))
		}
		if s.Items == nil {
			return invalidArgument(field, "items are not allowed")
		}
		for i, item := range v {
			if err := s.Items.validate(item, fmt.Sprintf("%s[%d]", field, i)); err != nil {
				return err
			}
		}
	case "integer":
		v, ok := value.(float64)
		if !ok || v != float64(int64(v)) {
			return invalidArgument(field, "must be an integer")
		}
		if s.Minimum != nil && v < *s.Minimum {
			return invalidArgument(field, fmt.Sprintf("must be at least %g", *s.Minimum))
		}
		if s.Maximum != nil && v > *s.Maximum {
			return invalidArgument(field, fmt.Sprintf("must be at most %g", *s.Maximum))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return invalidArgument(field, "must be a boolean")
		}
	default:
		return invalidArgument(field, "has an unsupported schema type")
	}
	return nil
}

func argumentField(parent, key string) string {
	if parent == "arguments" {
		return key
	}
	return parent + "." + key
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
