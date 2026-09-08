package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/explain"
)

func TestIssueExplanationIncludesAttemptDetail(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		attempt *explain.Attempt
		want    string
	}{
		{name: "no attempt"},
		{name: "no detail", attempt: &explain.Attempt{ID: 4885}},
		{name: "cancellation", attempt: &explain.Attempt{ID: 4885, StatusMessage: "worker cancellation: context_cancelled (source: orchestrator.release_claim)"}, want: "Attempt detail: worker cancellation: context_cancelled (source: orchestrator.release_claim)"},
		{name: "active attempt", attempt: &explain.Attempt{ID: 4894, StatusMessage: "waiting for validation"}, want: "Attempt detail: waiting for validation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := writeIssueExplanationPretty(&output, explain.IssueExplanation{Attempt: tt.attempt}); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if strings.Contains(output.String(), "Attempt detail:") {
					t.Fatalf("unexpected attempt detail: %s", output.String())
				}
			} else if !strings.Contains(output.String(), tt.want) {
				t.Fatalf("output = %s, want %s", output.String(), tt.want)
			}
		})
	}
}
