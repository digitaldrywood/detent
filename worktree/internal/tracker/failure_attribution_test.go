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
	for _, test := range []struct {
		name, outcome string
		failure       NativeTerminalFailure
	}{
		{"workspace_hook", "instance", NativeTerminalFailure{ErrorClass: "workspace_hook"}},
		{"validation", "", NativeTerminalFailure{ErrorClass: "validation"}},
		{"backend_startup", "instance", NativeTerminalFailure{ErrorClass: "backend_startup"}},
		{"protocol", "instance", NativeTerminalFailure{ErrorClass: "protocol"}},
		{"acceptance rejection", "instance", NativeTerminalFailure{Rejection: &PublicationRejection{Category: "acceptance_validation", Field: "validator.session_id"}}},
		{"audit rejection", "instance", NativeTerminalFailure{Rejection: &PublicationRejection{Category: "security_audit", Field: "run.exit_status"}}},
		{"event rejection", "instance", NativeTerminalFailure{Rejection: &PublicationRejection{Category: "event_validation", Field: "data.runtime"}}},
		{"audit publication", "instance", NativeTerminalFailure{Publication: &PublicationFailure{Operation: "security_audit/review", Code: "request_failed"}}},
		{"invalid rejection", "", NativeTerminalFailure{Rejection: &PublicationRejection{Category: "security_audit", Field: "private"}}},
		{"invalid publication", "", NativeTerminalFailure{Publication: &PublicationFailure{Operation: "private", Code: "request_failed"}}},
	} {
		for _, outcome := range []string{"", "issue", "unknown"} {
			input := test.failure
			if outcome != "" {
				input.Decision = &FailureAttribution{Outcome: outcome, Reason: "prior decision"}
			}
			want := test.outcome
			if want == "" {
				want = outcome
			}
			if failure := input.Public(); failure.Attribution != want {
				t.Fatalf("%s with decision %s: attribution=%s, want %s", test.name, outcome, failure.Attribution, want)
			} else if again := failure.Public(); !reflect.DeepEqual(again, failure) {
				t.Fatalf("%s changed attribution on replay", test.name)
			}
		}
	}
}
