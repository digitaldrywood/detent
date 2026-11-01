package api

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
			issue := IssueFromNative(tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: "wi_done"}, State: "Done", Terminal: test.terminal, ClosedAt: &closed})
			if issue.Closed != test.terminal || (issue.ClosedAt == nil) != (test.want == nil) || test.want != nil && !issue.ClosedAt.Equal(*test.want) {
				t.Fatalf("closed=%t closed_at=%v, want closed=%t closed_at=%v", issue.Closed, issue.ClosedAt, test.terminal, test.want)
			}
		})
	}
}

func TestIssueFromNativeCarriesChangeWorkProduct(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change *tracker.NativeListChange
		want   string
	}{
		{name: "no source"},
		{name: "unpublished Change", change: &tracker.NativeListChange{HeadSHA: "head"}, want: "head"},
		{name: "landing Change", change: &tracker.NativeListChange{HeadSHA: "head", LandingPending: true}, want: "head"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := IssueFromNative(tracker.NativeIssue{ListChange: test.change})
			if issue.NativeChangeHeadSHA != test.want {
				t.Fatalf("work product head = %q, want %q", issue.NativeChangeHeadSHA, test.want)
			}
		})
	}
}
