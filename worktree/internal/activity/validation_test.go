package activity

import "testing"

func TestValidationPhase(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, text, want string
	}{
		{"ordinary coding", "updating validation tests", ""},
		{"remote CI", "waiting for GitHub checks", ""},
		{"queue", "validation gate waiting: position=3 queued=4 waited=5m", "waiting_validation"},
		{"started", "validation gate running: owner_pid=10 waited=5m", "validating"},
		{"old client", "validation gate acquired shared lock; starting validation", "validating"},
		{"handoff", "validation gate waiting: position=1\nvalidation gate running: owner_pid=10", "validating"},
		{"completed", "validation gate running: owner_pid=10\nvalidation gate finished: result=passed", ""},
		{"failed", "validation gate waiting: position=2\nacquire validation lock: validation queue wait ended", ""},
		{"canceled before start", "validation gate waiting: position=1\nvalidation canceled before command start: context canceled", ""},
		{"restarted", "validation gate finished: result=failed\nvalidation gate waiting: position=1", "waiting_validation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ValidationPhase(tt.text); got != tt.want {
				t.Fatalf("phase = %q, want %q", got, tt.want)
			}
		})
	}
}
