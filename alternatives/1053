package explain

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestExplanationDependencySources(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"native", "prose", ""} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			issue := telemetry.Issue{DependencyNotes: []string{"owner/repo#99: prose dependency ignored: native relation absent"}}
			if source != "" {
				issue.BlockedBy = []telemetry.BlockedRef{{Identifier: "owner/repo#100", Source: source}}
			}
			got := buildExplanation(time.Now(), Identity{}, true, collectedEvidence{snapshotIssues: []snapshotIssue{{issue: issue}}})
			if len(got.Dependencies) != len(issue.BlockedBy) {
				t.Fatalf("Dependencies = %+v", got.Dependencies)
			}
			if source != "" && got.Dependencies[0].Source != source {
				t.Fatalf("source = %q, want %q", got.Dependencies[0].Source, source)
			}
			if len(got.DependencyNotes) != 1 || got.DependencyNotes[0] != issue.DependencyNotes[0] {
				t.Fatalf("DependencyNotes = %v", got.DependencyNotes)
			}
		})
	}
}
