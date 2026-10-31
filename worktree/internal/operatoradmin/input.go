package operatoradmin

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type fieldSchema struct {
	Type       string                 `json:"type"`
	MinLength  int                    `json:"minLength"`
	MaxLength  int                    `json:"maxLength"`
	Minimum    int                    `json:"minimum"`
	Maximum    int                    `json:"maximum"`
	MinItems   int                    `json:"minItems"`
	MaxItems   int                    `json:"maxItems"`
	Enum       []string               `json:"enum"`
	Items      *fieldSchema           `json:"items"`
	Properties map[string]fieldSchema `json:"properties"`
	Required   []string               `json:"required"`
}

// Decode enforces each named tool's bounded fields before producing typed
// application input. A field belonging to another operation is still rejected.
func Decode(name string, raw json.RawMessage) (Input, error) {
	d, ok := operatortool.Lookup(name)
	if !ok || !operatortool.IsAdministration(name) || len(raw) > operatortool.MaxArgumentBytes {
		return Input{}, operatortool.ErrInvalidArguments
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var schema struct {
		Properties map[string]fieldSchema `json:"properties"`
		Required   []string               `json:"required"`
	}
	if json.Unmarshal(d.InputSchema, &schema) != nil {
		return Input{}, ErrUnavailable
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Input{}, operatortool.ErrInvalidArguments
	}
	for _, key := range schema.Required {
		if _, ok := fields[key]; !ok {
			return Input{}, operatortool.ErrInvalidArguments
		}
	}
	for key, value := range fields {
		spec, ok := schema.Properties[key]
		if !ok || !validField(value, spec) {
			return Input{}, operatortool.ErrInvalidArguments
		}
	}
	var input Input
	if err := operatortool.DecodeArguments(raw, &input); err != nil {
		return Input{}, err
	}
	if input.Limit == 0 && (name == operatortool.OrganizationList || name == operatortool.MembershipList || name == operatortool.CredentialList) {
		input.Limit = operatortool.DefaultItemLimit
	}
	return input, nil
}

func validField(raw json.RawMessage, s fieldSchema) bool {
	if string(raw) == "null" {
		return false
	}
	switch s.Type {
	case "object":
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil || fields == nil {
			return false
		}
		for _, key := range s.Required {
			if _, ok := fields[key]; !ok {
				return false
			}
		}
		for key, value := range fields {
			spec, ok := s.Properties[key]
			if !ok || !validField(value, spec) {
				return false
			}
		}
		return true
	case "string":
		var value string
		if json.Unmarshal(raw, &value) != nil || len(value) < s.MinLength || s.MaxLength > 0 && len(value) > s.MaxLength || s.MinLength > 0 && strings.TrimSpace(value) == "" {
			return false
		}
		if len(s.Enum) > 0 {
			for _, v := range s.Enum {
				if v == value {
					return true
				}
			}
			return false
		}
		return true
	case "integer":
		var value int
		return json.Unmarshal(raw, &value) == nil && value >= s.Minimum && (s.Maximum == 0 || value <= s.Maximum)
	case "boolean":
		var value bool
		return json.Unmarshal(raw, &value) == nil
	case "array":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || len(values) < s.MinItems || len(values) > s.MaxItems || s.Items == nil {
			return false
		}
		for _, v := range values {
			if !validField(v, *s.Items) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

type PageResult[T any] struct {
	Items      []T  `json:"items"`
	NextOffset *int `json:"next_offset,omitempty"`
}

// Page bounds retained application rows; callers must sort before paging.
func Page[T any](rows []T, in Input) PageResult[T] {
	result := PageResult[T]{Items: []T{}}
	if in.Offset >= len(rows) {
		return result
	}
	limit := in.Limit
	if limit < 1 || limit > operatortool.MaxItemLimit {
		limit = operatortool.DefaultItemLimit
	}
	end := min(in.Offset+limit, len(rows))
	result.Items = rows[in.Offset:end]
	if end < len(rows) {
		result.NextOffset = &end
	}
	return result
}
