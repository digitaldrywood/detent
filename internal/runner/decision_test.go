package runner

import (
	"strings"
	"testing"
)

func TestNativeHumanCompletionRequiresDecision(t *testing.T) {
	for _, tt := range []struct {
		name, action string
		invalid      bool
	}{
		{"human question", "human_action: Choose a name", true},
		{"dependency", "blockers:\n  - ref: '#42'\n    reason: dependency must finish", false},
		{"instance", "reason_code: instance_limitation", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := "```detent-status\nschema: 1\nstatus: blocked\n" + tt.action + "\n```"
			signal, reported := NativeCompletionSignalFromMessage(body)
			if !reported || signal == nil || (signal.Invalid != nil) != tt.invalid {
				t.Fatalf("signal = %#v, reported = %v", signal, reported)
			}
			if tt.invalid {
				d := NativeDispositionFromMessage(body)
				if d == nil || d.Status != "in_progress" || d.HumanAction || d.ReasonCode != "invalid_workpad_signal" || !strings.Contains(d.FinalSummary, "requires decision_request") {
					t.Fatalf("refusal = %#v", d)
				}
			}
		})
	}
}

const testDecisionRequestYAML = `decision_request:
  summary: The work is ready for a decision.
  question: Which path should the implementation take?
  options:
    - label: Proceed with the change
      consequence: Resume implementation with the requested change.
      recommended: true
      return_state: Rework
    - label: Revise the approach
      consequence: Resume planning with a revised approach.
      return_state: Todo
  custom_allowed: true
  custom_return_state: Rework
`
