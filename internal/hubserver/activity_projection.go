package hubserver

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func activityRunRole(data tracker.NativeRunData) string {
	if data.Runtime != nil && data.Runtime.Identity.Role != "" {
		return data.Runtime.Identity.Role
	}
	if data.Identity != nil {
		return data.Identity.Role
	}
	return ""
}

func activityRunPhases(data tracker.NativeRunData) ([]tracker.NativePhase, int) {
	if data.Runtime == nil {
		return nil, 0
	}
	return data.Runtime.Phases, data.Runtime.PhasesDropped
}

func decodeActivityRun(ctx context.Context, q nativeQueryer, raw string, data *tracker.NativeRunData) error {
	var document map[string]json.RawMessage
	if err := decodeStorageJSON(ctx, []byte(raw), &document); err != nil {
		return err
	}
	if document == nil {
		return decodeRunDetail(ctx, q, raw, data)
	}
	var runtime map[string]json.RawMessage
	if value := document["runtime"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &runtime); err != nil {
			return err
		}
		var phases []tracker.NativePhase
		if value := runtime["phases"]; len(value) > 0 && json.Unmarshal(value, &phases) != nil {
			delete(runtime, "phases")
			encoded, err := json.Marshal(runtime)
			if err != nil {
				return err
			}
			document["runtime"] = encoded
			encoded, err = json.Marshal(document)
			if err != nil {
				return err
			}
			raw = string(encoded)
		}
	}
	return decodeRunDetail(ctx, q, raw, data)
}
