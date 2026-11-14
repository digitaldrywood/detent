package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
)

const recoveryTestToken = "fresh-recovery-administrator-token-example"

func TestRecoveryMergeHistoryBoundReportsPossibleOmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		page      []eventstore.Event
		index     int
		pageLimit int
		want      bool
	}{
		{name: "later candidate in page", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "workflow.transitioned", FromState: "merging"}}, index: 0, pageLimit: 2, want: true},
		{name: "short final page", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "comment.created"}}, index: 0, pageLimit: 3, want: false},
		{name: "full page may have older candidate", page: []eventstore.Event{{Type: "workflow.transitioned"}, {Type: "comment.created"}}, index: 0, pageLimit: 2, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := recoveryMergeHistoryPageHasMore(test.page, test.index, test.pageLimit); got != test.want {
				t.Fatalf("recoveryMergeHistoryPageHasMore() = %t, want %t", got, test.want)
			}
		})
	}
}
