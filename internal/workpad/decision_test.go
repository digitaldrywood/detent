package workpad

import (
	"strings"
	"testing"
)

func TestDecisionRequestStatusBlock(t *testing.T) {
	valid := `schema: 1
status: blocked
human_action: Choose a service name
decision_request:
  summary: The implementation is ready for the naming decision.
  question: Which service name should the installer use?
  options:
    - label: Use a derived name
      consequence: New installs get a name derived from the Hub URL.
      recommended: true
      return_state: Rework
    - label: Require an explicit name
      consequence: New installs require an operator-supplied name.
      recommended: false
      return_state: Todo
  custom_allowed: true
  custom_return_state: Rework
`
	for _, tt := range []struct{ name, body, want string }{
		{"valid", valid, ""},
		{"missing question", strings.Replace(valid, "  question: Which service name should the installer use?\n", "", 1), "question"},
		{"zero options", strings.Replace(valid, valid[strings.Index(valid, "  options:"):strings.Index(valid, "  custom_allowed:")], "  options: []\n", 1), "two to four"},
		{"five options", strings.Replace(valid, "  custom_allowed:", strings.Repeat("    - label: Another name\n      consequence: Use a different name.\n      return_state: Todo\n", 3)+"  custom_allowed:", 1), "two to four"},
		{"no recommendation", strings.Replace(valid, "recommended: true", "recommended: false", 1), "exactly one"},
		{"multiple recommendations", strings.ReplaceAll(valid, "recommended: false", "recommended: true"), "exactly one"},
		{"custom disabled", strings.Replace(valid, "custom_allowed: true", "custom_allowed: false", 1), "custom_allowed"},
		{"unsafe destination", strings.Replace(valid, "return_state: Todo", "return_state: Done", 1), "return_state"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			signal, err := ParseStatusBlock(tt.body, "")
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if signal.DecisionRequest == nil || signal.DecisionRequest.Question != "Which service name should the installer use?" || len(signal.DecisionRequest.Options) != 2 {
				t.Fatalf("request = %#v", signal.DecisionRequest)
			}
		})
	}
}
