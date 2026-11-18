package reportdata

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func DecodeRun(ctx context.Context, raw string, data *tracker.NativeRunData, decode func(context.Context, []byte, any) error, detail func(context.Context, string, *tracker.NativeRunData) error) error {
	var document map[string]json.RawMessage
	if err := decode(ctx, []byte(raw), &document); err != nil {
		return err
	}
	if document == nil {
		return detail(ctx, raw, data)
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
	return detail(ctx, raw, data)
}
