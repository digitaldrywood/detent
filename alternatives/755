package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestIssueExplanationDependencySources(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"native", "prose"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			note := "owner/repo#99: prose dependency ignored: native relation absent"
			result := explain.IssueExplanation{Dependencies: []telemetry.BlockedRef{{Identifier: "owner/repo#100", Source: source}}, DependencyNotes: []string{note}}
			if err := writeIssueExplanationPretty(&output, result); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Dependency: owner/repo#100 [" + source + "]", note} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("explanation missing %q", want)
				}
			}
		})
	}
}
