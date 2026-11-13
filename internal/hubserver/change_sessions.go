package hubserver

import (
	"database/sql"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeImplementingSession(run tracker.NativeRunData) int64 {
	return nativeRuntimeSession(run.Runtime)
}

func nativeRuntimeSession(runtime *tracker.NativeRuntimeObservation) int64 {
	if runtime == nil || runtime.Activity == nil {
		return -1
	}
	return runtime.Activity.SessionID
}

type changeSessionAuthority struct {
	session sql.NullInt64
	machine tracker.MachineID
	raw     string
}
