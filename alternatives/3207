package tracker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestFailureAttributionPublicRecord(t *testing.T) {
	for _, test := range []struct {
		name, outcome string
		valid         bool
	}{
		{"instance", "instance", true}, {"issue", "issue", true}, {"unknown", "unknown", true}, {"unsupported", "park", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := FailureAttribution{Outcome: test.outcome, Reason: "explanation", Evidence: FailureEvidence{Step: "validation", ExitCode: new(137), OutputTail: "first line\nOOM killed token=secret-value", SourceDiff: "diff --git a/detent.yaml b/detent.yaml\n+after_create: ./setup.sh"}}
			failure := (NativeTerminalFailure{Decision: &input}).Public()
			if !test.valid {
				if failure.Decision != nil || failure.Attribution != "" {
					t.Fatal("invalid decision retained")
				}
				return
			}
			data, err := json.Marshal(failure)
			if err != nil {
				t.Fatal(err)
			}
			var stored NativeTerminalFailure
			if err := json.Unmarshal(data, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.Attribution != test.outcome || stored.Decision == nil || !strings.Contains(stored.Decision.Evidence.SourceDiff, "+after_create") || !strings.Contains(stored.Decision.Evidence.OutputTail, "OOM killed") || strings.Contains(stored.Decision.Evidence.OutputTail, "secret-value") || *stored.Decision.Evidence.ExitCode != 137 {
				t.Fatalf("lost or unsafe decision: %+v", stored.Decision)
			}
			if again := stored.Public(); !reflect.DeepEqual(again, stored) {
				t.Fatal("public record changes on replay")
			}
		})
	}
	for _, class := range []string{"workspace_hook", "validation", "backend_startup", "protocol"} {
		if failure := (NativeTerminalFailure{ErrorClass: class}).Public(); failure.Attribution != "" {
			t.Fatalf("error class %s inferred blame", class)
		}
	}
}
