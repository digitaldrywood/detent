package github

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var (
	graphQLPageSize         = regexp.MustCompile(`\b(?:first|last)\s*:\s*([0-9]+)\b`)
	graphQLVariablePageSize = regexp.MustCompile(`\b(?:first|last)\s*:\s*\$([A-Za-z_][A-Za-z_0-9]*)`)
	graphQLIDVariable       = regexp.MustCompile(`\bids\s*:\s*\$([A-Za-z_][A-Za-z_0-9]*)`)
	graphQLSingleID         = regexp.MustCompile(`\bid\s*:`)
)

func graphQLOperationName(query, fallback string) string {
	parts := strings.Fields(firstLine(query))
	if len(parts) >= 2 && (parts[0] == "query" || parts[0] == "mutation") {
		name := parts[1]
		if index := strings.IndexByte(name, '('); index >= 0 {
			name = name[:index]
		}
		return name
	}
	return fallback
}

// requestedGraphQLNodes counts declared node slots, multiplying nested
// connections by their parent capacity. It describes query capacity, not
// GitHub's opaque cost formula.
func requestedGraphQLNodes(query string, variables map[string]any) int64 {
	factor := int64(1)
	stack := []int64{}
	var total int64
	var pending int64
	for i := 0; i < len(query); i++ {
		switch query[i] {
		case '(':
			start := i
			depth := 1
			for i++; i < len(query) && depth > 0; i++ {
				switch query[i] {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			if depth != 0 {
				return total
			}
			field := graphQLFieldBefore(query[:start])
			pending = graphQLFieldCapacity(field, query[start+1:i-1], variables)
			i--
		case '{':
			stack = append(stack, factor)
			if pending > 0 {
				total += factor * pending
				factor *= pending
			}
			pending = 0
		case '}':
			if len(stack) > 0 {
				factor = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			pending = 0
		}
	}
	if total == 0 {
		return 1
	}
	return total
}

func graphQLFieldBefore(prefix string) string {
	i := len(prefix) - 1
	for i >= 0 && (prefix[i] == ' ' || prefix[i] == '\n' || prefix[i] == '\t') {
		i--
	}
	end := i + 1
	for i >= 0 && (prefix[i] == '_' || prefix[i] >= 'A' && prefix[i] <= 'Z' || prefix[i] >= 'a' && prefix[i] <= 'z') {
		i--
	}
	return prefix[i+1 : end]
}

func graphQLFieldCapacity(field, args string, variables map[string]any) int64 {
	if field == "node" && graphQLSingleID.MatchString(args) {
		return 1
	}
	if field == "nodes" {
		if match := graphQLIDVariable.FindStringSubmatch(args); len(match) == 2 {
			value := reflect.ValueOf(variables[match[1]])
			if value.IsValid() && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
				return int64(value.Len())
			}
		}
	}
	if match := graphQLPageSize.FindStringSubmatch(args); len(match) == 2 {
		limit, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil {
			return limit
		}
	}
	if match := graphQLVariablePageSize.FindStringSubmatch(args); len(match) == 2 {
		switch value := variables[match[1]].(type) {
		case int:
			return int64(value)
		case int64:
			return value
		}
	}
	return 0
}

func returnedGraphQLNodes(data json.RawMessage) int64 {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return 0
	}
	return countReturnedGraphQLNodes(value, false)
}

func countReturnedGraphQLNodes(value any, counted bool) int64 {
	object, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	var count int64
	if _, hasID := object["id"]; hasID && !counted {
		count++
	}
	for key, child := range object {
		switch v := child.(type) {
		case []any:
			if key == "nodes" {
				for _, item := range v {
					if item != nil {
						count++
					}
				}
			}
			for _, item := range v {
				count += countReturnedGraphQLNodes(item, key == "nodes")
			}
		case map[string]any:
			if key == "node" {
				count++
			}
			count += countReturnedGraphQLNodes(v, key == "node")
		}
	}
	return count
}
