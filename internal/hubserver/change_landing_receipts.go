package hubserver

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type landingReceiptDecodeError struct{}

func (*landingReceiptDecodeError) Error() string {
	return "landing receipt missing or empty in attempt object"
}

func (*landingReceiptDecodeError) Unwrap() error { return artifact.ErrIntegrity }

func decodeLandingReceipt(ctx context.Context, raw string, object bool) (tracker.NativeLandingReceipt, error) {
	if object {
		var run tracker.NativeRunData
		if err := decodeStorageJSON(ctx, []byte(raw), &run); err != nil {
			return tracker.NativeLandingReceipt{}, err
		}
		if run.Runtime == nil || run.Runtime.Landing == nil || reflect.DeepEqual(*run.Runtime.Landing, tracker.NativeLandingReceipt{}) {
			return tracker.NativeLandingReceipt{}, &landingReceiptDecodeError{}
		}
		return *run.Runtime.Landing, nil
	}
	var receipt tracker.NativeLandingReceipt
	err := decodeStorageJSON(ctx, []byte(raw), &receipt)
	return receipt, err
}

func attemptLandingPayload(document []byte, receipt json.RawMessage) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		return nil, err
	}
	if object == nil {
		object = map[string]json.RawMessage{}
	}
	var runtime map[string]json.RawMessage
	if value := object["runtime"]; len(value) > 0 && string(value) != "null" {
		if err := json.Unmarshal(value, &runtime); err != nil {
			return nil, err
		}
	}
	if runtime == nil {
		runtime = map[string]json.RawMessage{}
	}
	runtime["landing"] = receipt
	encodedRuntime, err := json.Marshal(runtime)
	if err != nil {
		return nil, err
	}
	object["runtime"] = encodedRuntime
	return json.Marshal(object)
}
