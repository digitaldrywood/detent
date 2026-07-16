package templates

import (
	"strings"
	"testing"
)

func TestStopRunDialogPreservesConfiguredDefault(t *testing.T) {
	t.Parallel()

	html := renderBoardComponent(t, StopRunDialogContent(StopRunDialogData{
		ProjectID:   "detent",
		IssueID:     "issue-1354",
		Identifier:  "digitaldrywood/detent#1354",
		Destination: "Paused",
		CanSubmit:   true,
	}))
	for _, want := range []string{`value="Paused"`, `checked`, "Configured default · Paused", "Preserve the existing stop-run destination"} {
		if !strings.Contains(html, want) {
			t.Fatalf("dialog missing configured default %q:\n%s", want, html)
		}
	}
}
