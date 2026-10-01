package serviceapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	StateProjection      = "cli"
	StateCollectionLimit = 100
	// Leave headroom below the client's hard 1 MiB transport bound.
	StateResponseBytes = 768 << 10
	StateValueBytes    = 16 << 10
)

type StateTruncation struct {
	Limit         int                         `json:"limit"`
	MaxBytes      int                         `json:"max_bytes,omitempty"`
	ValueMaxBytes int                         `json:"value_max_bytes,omitempty"`
	Truncated     bool                        `json:"truncated"`
	Collections   []StateCollectionTruncation `json:"collections"`
	OmittedFields []StateFieldOmission        `json:"omitted_fields,omitempty"`
}

type StateCollectionTruncation struct {
	Path     string `json:"path"`
	Omitted  int    `json:"omitted"`
	Total    int    `json:"total,omitempty"`
	Returned int    `json:"returned"`
}

type StateFieldOmission struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Total  *int   `json:"total,omitempty"`
}

// BoundedState projects the public state model before it is sent over HTTP.
// Budgets include omission metadata. A discarded subtree gets one omission
// record. Collection records describe arrays in the returned projection;
// field records name omitted subtrees, without retaining their nested records.
func BoundedState(response any) (map[string]any, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode state projection: %w", err)
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode state projection: %w", err)
	}
	delete(payload, "board_issues")
	node, ok := boundStateValue(payload, "", StateResponseBytes)
	if !ok {
		return nil, fmt.Errorf("state summary exceeds projection budget")
	}
	payload = node.value.(map[string]any)
	if node.collections == nil {
		node.collections = []StateCollectionTruncation{}
	}
	slices.SortFunc(node.collections, func(a, b StateCollectionTruncation) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(node.fields, func(a, b StateFieldOmission) int { return strings.Compare(a.Path, b.Path) })
	payload["truncation"] = StateTruncation{
		Limit: StateCollectionLimit, MaxBytes: StateResponseBytes, ValueMaxBytes: StateValueBytes,
		Truncated:   len(node.collections) > 0 || len(node.fields) > 0,
		Collections: node.collections, OmittedFields: node.fields,
	}
	return payload, nil
}

type stateNode struct {
	value       any
	collections []StateCollectionTruncation
	fields      []StateFieldOmission
}

func (n stateNode) size() int {
	// Values originate from decoded JSON; marshaling them cannot fail.
	value, _ := json.Marshal(n.value)
	collections, _ := json.Marshal(n.collections)
	fields, _ := json.Marshal(n.fields)
	// Includes the envelope's keys, limits, punctuation and booleans.
	return len(value) + len(collections) + len(fields) + 256
}

func boundStateValue(value any, path string, budget int) (stateNode, bool) {
	node := stateNode{value: value, collections: []StateCollectionTruncation{}}
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		node.value = out
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		// Scalar summary fields (including refresh status) precede arrays,
		// so a large detail collection cannot consume their budget.
		slices.SortStableFunc(keys, func(a, b string) int {
			rank := func(key string) int {
				switch value[key].(type) {
				case map[string]any:
					return 1
				case []any:
					return 2
				default:
					return 0
				}
			}
			return rank(a) - rank(b)
		})
		if path == "" {
			// Preserve summary/freshness before spending the budget on details.
			priority := []string{"generated_at", "snapshot_age_seconds", "status", "counts", "enrichment", "instance", "shutdown", "update", "codex_totals", "lifetime_totals", "throughput", "budget", "memory_pressure", "io_pressure", "cpu_pressure", "rate_limits", "refresh"}
			slices.SortStableFunc(keys, func(a, b string) int {
				rank := func(key string) int {
					i := slices.Index(priority, key)
					if i < 0 {
						return len(priority)
					}
					return i
				}
				return rank(a) - rank(b)
			})
		}
		// Reserve every possible field omission before admitting any data.
		for _, key := range keys {
			omission := StateFieldOmission{Path: path + "/" + statePointerToken(key), Reason: "byte_limit"}
			if collection, ok := value[key].([]any); ok {
				total := len(collection)
				omission.Total = &total
			}
			node.fields = append(node.fields, omission)
		}
		if node.size() > budget {
			return stateNode{}, false
		}
		for _, key := range keys {
			childPath := path + "/" + statePointerToken(key)
			child, ok := boundStateValue(value[key], childPath, budget-node.size())
			if !ok {
				continue
			}
			candidate := stateNode{value: out, collections: slices.Concat(node.collections, child.collections), fields: slices.Clone(node.fields)}
			candidate.fields = slices.DeleteFunc(candidate.fields, func(field StateFieldOmission) bool { return field.Path == childPath })
			candidate.fields = append(candidate.fields, child.fields...)
			out[key] = child.value
			if candidate.size() > budget {
				delete(out, key)
				continue
			}
			node = candidate
		}
	case []any:
		out := make([]any, 0, min(len(value), StateCollectionLimit))
		node.value = out
		node.collections = append(node.collections, StateCollectionTruncation{Path: path, Total: len(value), Omitted: len(value)})
		if node.size() > budget {
			return stateNode{}, false
		}
		for i := 0; i < min(len(value), StateCollectionLimit); i++ {
			child, ok := boundStateValue(value[i], path+"/"+strconv.Itoa(i), budget-node.size())
			if !ok {
				break
			}
			candidate := stateNode{value: append(out, child.value), collections: slices.Concat(node.collections, child.collections), fields: slices.Concat(node.fields, child.fields)}
			candidate.collections[0].Returned = i + 1
			candidate.collections[0].Omitted = len(value) - i - 1
			if candidate.size() > budget {
				break
			}
			out = candidate.value.([]any)
			node = candidate
		}
		if len(out) == len(value) {
			node.collections = node.collections[1:]
		}
	default:
		if node.size()-256 > StateValueBytes {
			return stateNode{}, false
		}
	}
	return node, node.size() <= budget
}

func statePointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
