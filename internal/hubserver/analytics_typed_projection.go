package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func analyticsDimension(values ...string) string {
	var key strings.Builder
	key.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			key.WriteByte(',')
		}
		key.WriteByte('"')
		for j := range len(value) {
			c := value[j]
			switch c {
			case '"', '\\':
				key.WriteByte('\\')
				key.WriteByte(c)
			case '\b':
				key.WriteString(`\b`)
			case '\t':
				key.WriteString(`\t`)
			case '\n':
				key.WriteString(`\n`)
			case '\f':
				key.WriteString(`\f`)
			case '\r':
				key.WriteString(`\r`)
			default:
				if c < 0x20 {
					key.WriteString(`\u00`)
					key.WriteByte("0123456789abcdef"[c>>4])
					key.WriteByte("0123456789abcdef"[c&0xf])
				} else {
					key.WriteByte(c)
				}
			}
		}
		key.WriteByte('"')
	}
	key.WriteByte(']')
	return key.String()
}

var analyticsProjectionFields = []struct{ column, key string }{
	{"work_item_id", "work_item_id"}, {"sequence", "sequence"}, {"type", "type"},
	{"recorded_at", "recorded_at"}, {"started_at", "started_at"}, {"updated_at", "updated_at"},
	{"attempt_id", "attempt_id"}, {"period", "period"}, {"sample_id", "ID"},
	{"sample_at", "At"}, {"sample_ns", "AtNS"}, {"sample_start_ns", "StartNS"}, {"sample_from_ns", "FromNS"},
	{"skip_source", "Source"}, {"skip_reason", "Reason"}, {"last_at", "last_at"}, {"last_ns", "last_ns"},
	{"issue_id", "id"}, {"native_id", "native_id"}, {"title", "title"}, {"created_at", "created_at"},
	{"workflow_state_id", "workflow_state_id"}, {"lease_id", "lease_id"}, {"status", "status"},
	{"provider", "provider"}, {"model", "model"}, {"input", "input"}, {"cached_input", "cached_input"},
	{"output", "output"}, {"currency", "currency"}, {"acquired_at", "acquired_at"},
	{"released_at", "released_at"}, {"expires_at", "expires_at"}, {"failed_at", "failed_at"},
	{"scheduler_outcome", "outcome"}, {"scheduler_source", "source"}, {"scheduler_reason", "reason"},
	{"analytics_to_state", "to_state"},
}

func analyticsProjection(raw string) ([]any, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, errors.New("decode analytics projection: invalid JSON object")
	}
	values := make([]any, 0, len(analyticsProjectionFields)+4)
	for _, field := range analyticsProjectionFields {
		var value any
		encoded := data[field.key]
		if field.column == "skip_source" && len(encoded) == 0 {
			encoded = data["source"]
		}
		if field.column == "skip_reason" && len(encoded) == 0 {
			encoded = data["reason"]
		}
		if field.column == "issue_id" && len(data["issue_id"]) > 0 {
			encoded = data["issue_id"]
		}
		if len(encoded) > 0 && string(encoded) != "null" {
			if strings.HasSuffix(field.column, "_ns") || field.column == "sequence" || field.column == "input" || field.column == "cached_input" || field.column == "output" {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = n
			} else if (field.column == "issue_id" || field.column == "workflow_state_id") && encoded[0] != '"' {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: expected integer or string", field.key)
				}
				value = strconv.FormatInt(n, 10)
			} else {
				var s string
				if err := json.Unmarshal(encoded, &s); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = s
			}
		}
		values = append(values, value)
	}
	var identity struct {
		Identity struct{ Role *string }
		Runtime  struct{ Identity struct{ Role *string } }
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return nil, errors.New("decode analytics projection: invalid identity or timestamp")
	}
	role := identity.Runtime.Identity.Role
	if role == nil {
		role = identity.Identity.Role
	}
	values = append(values, role, 1)
	for _, field := range []struct{ column, key string }{{"failed_day", "failed_at"}, {"recorded_day", "recorded_at"}} {
		var rawTime *string
		if len(data[field.key]) > 0 && string(data[field.key]) != "null" {
			if err := json.Unmarshal(data[field.key], &rawTime); err != nil {
				return nil, errors.New("decode analytics projection: invalid identity or timestamp")
			}
		}
		values = append(values, rawTime)
	}
	return values, nil
}
