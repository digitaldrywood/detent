package hubclient

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIssueFromNativeCarriesCompletionTime(t *testing.T) {
	t.Parallel()
	closed := time.Date(2026, 10, 10, 19, 16, 18, 0, time.UTC)
	for _, test := range []struct {
		name     string
		terminal bool
		want     *time.Time
	}{
		{name: "done", terminal: true, want: &closed},
		{name: "reopened", terminal: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := issueFromNative(tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_done"}, State: "Done", Terminal: test.terminal, ClosedAt: &closed})
			if issue.Closed != test.terminal || (issue.ClosedAt == nil) != (test.want == nil) || test.want != nil && !issue.ClosedAt.Equal(*test.want) {
				t.Fatalf("closed=%t closed_at=%v, want closed=%t closed_at=%v", issue.Closed, issue.ClosedAt, test.terminal, test.want)
			}
		})
	}
}
