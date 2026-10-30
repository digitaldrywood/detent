package progresscheck

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestFailureAttribution(t *testing.T) {
	yes, no := true, false
	for _, test := range []struct {
		name, outcome, reason string
		evidence              tracker.FailureEvidence
	}{
		{"changed workspace hook", "issue", "source changed the failing setup hook", tracker.FailureEvidence{Step: "after_create", OutputTail: "setup failed", ExitCode: new(2), SourceChangedConfiguration: &yes}},
		{"checkout disk full", "instance", "checkout exhausted instance disk", tracker.FailureEvidence{Step: "checkout", OutputTail: "no space left on device", ExitCode: new(128), SourceChangedConfiguration: &no}},
		{"validation OOM", "instance", "validation was killed by the instance OOM killer", tracker.FailureEvidence{Step: "validation", OutputTail: "OOM killer terminated validation", ExitCode: new(137), RecentOtherIssueSuccess: &yes}},
		{"deterministic test failure", "issue", "assertion fails on the supplied source", tracker.FailureEvidence{Step: "validation", OutputTail: "TestExample: expected 2 got 1", ExitCode: new(1)}},
		{"missing evidence", "unknown", "exit evidence unavailable", tracker.FailureEvidence{Step: "validation", MissingEvidence: "output unavailable"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			backend := ciBackend(func(_ context.Context, instructions, input string) (string, error) {
				calls++
				var got tracker.FailureEvidence
				if instructions != FailureInstructions || json.Unmarshal([]byte(input), &got) != nil || got.Step != test.evidence.Step || got.OutputTail != test.evidence.OutputTail {
					t.Fatal("failure evidence not sent as data")
				}
				output, _ := json.Marshal(map[string]string{"outcome": test.outcome, "reason": test.reason})
				return string(output), nil
			})
			result, err := (AI{Backend: backend}).AttributeFailure(t.Context(), test.evidence)
			if err != nil || calls != 1 || result.Outcome != test.outcome || result.Reason != test.reason || result.Evidence.OutputTail != test.evidence.OutputTail {
				t.Fatalf("decision=%+v calls=%d error=%v", result, calls, err)
			}
		})
	}
}

func TestFailureAttributionRejectsInvalidEvidenceAndAnswers(t *testing.T) {
	for _, test := range []struct {
		name, output string
		evidence     tracker.FailureEvidence
		fail         bool
		calls        int
	}{
		{name: "large source diff", evidence: tracker.FailureEvidence{SourceDiff: strings.Repeat("x", 4001)}, calls: 0},
		{name: "large evidence", evidence: tracker.FailureEvidence{OutputTail: strings.Repeat("x", 8001)}, calls: 0},
		{name: "large step", evidence: tracker.FailureEvidence{Step: strings.Repeat("x", 513)}, calls: 0},
		{name: "large output", output: strings.Repeat("x", MaxOutputBytes+1), calls: 1},
		{name: "missing reason", output: `{"outcome":"instance"}`, calls: 1},
		{name: "invalid outcome", output: `{"outcome":"park","reason":"none"}`, calls: 1},
		{name: "extra field", output: `{"outcome":"instance","reason":"disk","lane":"Blocked"}`, calls: 1},
		{name: "trailing data", output: `{"outcome":"instance","reason":"disk"} {}`, calls: 1},
		{name: "backend failure", fail: true, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			backend := ciBackend(func(context.Context, string, string) (string, error) {
				calls++
				if test.fail {
					return "", errors.New("backend unavailable")
				}
				return test.output, nil
			})
			result, err := (AI{Backend: backend}).AttributeFailure(t.Context(), test.evidence)
			if err == nil || result.Outcome != "unknown" || calls != test.calls {
				t.Fatalf("decision=%+v error=%v calls=%d", result, err, calls)
			}
		})
	}
}
