package recovery

import "github.com/digitaldrywood/detent/internal/tracker"

func ActiveAttempt(attempts []tracker.NativeAttempt, lease tracker.NativeLease) (tracker.NativeAttempt, bool) {
	for _, attempt := range attempts {
		if attempt.Identity != nil && attempt.LeaseID == lease.ID && attempt.FencingToken == lease.FencingToken && (attempt.Status == "running" || attempt.Status == "interrupted") {
			return attempt, true
		}
	}
	return tracker.NativeAttempt{}, false
}
