package activity

import "github.com/digitaldrywood/detent/internal/tracker"

func RunRole(data tracker.NativeRunData) string {
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
