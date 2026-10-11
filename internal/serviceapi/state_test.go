package serviceapi

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestBoundedState(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	for _, mode := range []string{"ordinary", "nested and oversized value", "total byte budget", "metadata budget"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			// Catches loss of counts/freshness and overflow of the response
			// budget by either moderate entries or their omission metadata.
			payload := map[string]any{
				"generated_at": "2026-10-01T05:51:00Z", "snapshot_age_seconds": 30,
				"counts":     map[string]any{"running": 150, "tokens": json.Number("9007199254740993")},
				"refresh":    map[string]any{"status": "degraded"},
				"enrichment": map[string]any{"status": "omitted"},
				"running":    []any{},
			}
			if mode != "ordinary" {
				rows := make([]any, 150)
				for i := range rows {
					row := map[string]any{"index": i}
					switch mode {
					case "nested and oversized value":
						if i == 0 {
							row["body"] = strings.Repeat("x", 1<<20)
							row["a~/b"] = make([]any, 103)
						}
					case "total byte budget":
						row["body"] = strings.Repeat("x", 12<<10)
					case "metadata budget":
						for j := range 100 {
							row[fmt.Sprintf("nested%d", j)] = make([]any, 101)
						}
					}
					rows[i] = row
				}
				payload["running"] = rows
				payload["zz_remaining"] = []any{1, 2, 3}
			}
			projected, err := BoundedState(payload)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > StateResponseBytes {
				t.Fatalf("response has %d bytes, limit %d", len(encoded), StateResponseBytes)
			}
			metadata := projected["truncation"].(StateTruncation)
			if metadata.Truncated != (mode != "ordinary") {
				t.Fatalf("truncated = %t", metadata.Truncated)
			}
			if projected["generated_at"] != payload["generated_at"] || projected["snapshot_age_seconds"] != json.Number("30") {
				t.Fatal("generated time/freshness lost")
			}
			counts := projected["counts"].(map[string]any)
			if counts["running"] != json.Number("150") || counts["tokens"] != json.Number("9007199254740993") {
				t.Fatalf("counts = %v", counts)
			}
			for _, record := range metadata.Collections {
				array := stateTestPointer(t, projected, record.Path).([]any)
				original := stateTestPointer(t, payload, record.Path).([]any)
				if len(array) != record.Returned || len(original) != record.Total || record.Omitted != record.Total-record.Returned || len(array) > StateCollectionLimit {
					t.Fatalf("inaccurate collection metadata: %+v", record)
				}
			}
			for _, record := range metadata.OmittedFields {
				i := strings.LastIndex(record.Path, "/")
				parent := stateTestPointer(t, projected, record.Path[:i]).(map[string]any)
				key := strings.ReplaceAll(strings.ReplaceAll(record.Path[i+1:], "~1", "/"), "~0", "~")
				if _, exists := parent[key]; exists {
					t.Fatalf("omitted field still present: %s", record.Path)
				}
				if array, ok := stateTestPointer(t, payload, record.Path).([]any); ok && (record.Total == nil || *record.Total != len(array)) {
					t.Fatalf("missing original array total: %+v", record)
				}
			}
			if mode == "nested and oversized value" && (len(metadata.OmittedFields) != 1 || metadata.OmittedFields[0].Path != "/running/0/body") {
				t.Fatalf("oversized value omissions: %+v", metadata.OmittedFields)
			}
			if mode == "total byte budget" && len(metadata.OmittedFields) == 0 && metadata.Collections[0].Returned == 100 {
				t.Fatal("total byte budget did not bound moderate entries")
			}
			t.Logf("response = %d bytes, %d collection records, %d field omissions", len(encoded), len(metadata.Collections), len(metadata.OmittedFields))
		})
	}
}

func stateTestPointer(t *testing.T, value any, path string) any {
	t.Helper()
	if path == "" {
		return value
	}
	for _, token := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch typed := value.(type) {
		case map[string]any:
			value = typed[token]
		case []any:
			i, err := strconv.Atoi(token)
			if err != nil || i < 0 || i >= len(typed) {
				t.Fatalf("invalid pointer %s", path)
			}
			value = typed[i]
		default:
			t.Fatalf("invalid pointer %s", path)
		}
	}
	return value
}
