package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
)

type smokeSchema struct {
	Type                 any                    `json:"type"`
	Properties           map[string]smokeSchema `json:"properties"`
	Required             []string               `json:"required"`
	Enum                 []any                  `json:"enum"`
	Format               string                 `json:"format"`
	Ref                  string                 `json:"$ref"`
	Defs                 map[string]smokeSchema `json:"$defs"`
	ExclusiveMinimum     *float64               `json:"exclusiveMinimum"`
	Pattern              string                 `json:"pattern"`
	Minimum              float64                `json:"minimum"`
	Maximum              *float64               `json:"maximum"`
	MinLength            int                    `json:"minLength"`
	MaxLength            *int                   `json:"maxLength"`
	MinItems             int                    `json:"minItems"`
	MaxItems             *int                   `json:"maxItems"`
	AnyOf                []smokeSchema          `json:"anyOf"`
	OneOf                []smokeSchema          `json:"oneOf"`
	Items                *smokeSchema           `json:"items"`
	AdditionalProperties json.RawMessage        `json:"additionalProperties"`
}

func smokeValue(schema smokeSchema, all bool) (any, error) {
	if len(schema.Enum) != 0 {
		return schema.Enum[0], nil
	}
	kind := ""
	if value, ok := schema.Type.(string); ok {
		kind = value
	}
	if types, ok := schema.Type.([]any); ok {
		for _, value := range types {
			if value != "null" {
				text, ok := value.(string)
				if !ok {
					return nil, fmt.Errorf("unsupported schema type %v", value)
				}
				kind = text
				break
			}
		}
	}
	if schema.Ref != "" {
		return map[string]any{}, nil
	}
	switch kind {
	case "", "object":
		for _, alternatives := range [][]smokeSchema{schema.AnyOf, schema.OneOf} {
			if len(alternatives) != 0 {
				schema.Required = append(slices.Clone(schema.Required), alternatives[0].Required...)
			}
		}
		fields := map[string]any{}
		for name, child := range schema.Properties {
			if !all && !slices.Contains(schema.Required, name) {
				continue
			}
			value, err := smokeValue(child, all)
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
			var child smokeSchema
			if err := json.Unmarshal(schema.AdditionalProperties, &child); err != nil {
				return nil, err
			}
			value, err := smokeValue(child, all)
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
		if schema.Pattern != "" {
			pattern, err := syntax.Parse(schema.Pattern, syntax.Perl)
			if err != nil {
				return nil, err
			}
			value, err = smokePattern(pattern)
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
			value, err := smokeValue(*schema.Items, all)
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

func smokePattern(pattern *syntax.Regexp) (string, error) {
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
		value, err := smokePattern(pattern.Sub[0])
		if pattern.Op == syntax.OpRepeat {
			value = strings.Repeat(value, pattern.Min)
		}
		return value, err
	case syntax.OpStar, syntax.OpQuest:
		return "", nil
	case syntax.OpConcat:
		var result strings.Builder
		for _, child := range pattern.Sub {
			value, err := smokePattern(child)
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
