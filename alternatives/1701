package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPreProviderFailureDecisionRetainsUsage(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name, outcome string
		allow         bool
	}{
		{"instance", "instance", true}, {"issue", "issue", true}, {"unknown", "unknown", true}, {"invalid", "park", false}, {"absent", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := &tracker.NativeTerminalFailure{ObservedAt: now}
			if test.outcome != "" {
				failure.Decision = &tracker.FailureAttribution{Outcome: test.outcome, Reason: "recorded decision", Evidence: tracker.FailureEvidence{Step: "checkout"}}
			}
			data := tracker.NativeRunData{Sequence: 1, Outcome: "failed", TerminalFailure: failure, Runtime: &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1, HeartbeatAt: now, Phase: "completed"}, Usage: []tracker.NativeUsage{{Provider: "example", Model: "example", Input: 10, Output: 1}}}
			err := validateNativeExecution(data, "run.finished")
			if (err == nil) != test.allow {
				t.Fatalf("allowed=%v error=%v", test.allow, err)
			}
			if test.allow && (failure.Stage != "pre_provider" || failure.Attribution != test.outcome) {
				t.Fatalf("attribution lost: %+v", failure)
			}
		})
	}
}
