package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeRecordedRecovery struct {
	attempt tracker.NativeAttempt
	prior   string
}
