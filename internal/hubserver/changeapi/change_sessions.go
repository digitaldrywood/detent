package changeapi

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeImplementingSession(run tracker.NativeRunData) int64 {
	return NativeRuntimeSession(run.Runtime)
}

func NativeRuntimeSession(runtime *tracker.NativeRuntimeObservation) int64 {
	if runtime == nil || runtime.Activity == nil {
		return -1
	}
	return runtime.Activity.SessionID
}
