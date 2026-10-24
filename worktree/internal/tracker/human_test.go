package tracker

import (
	"strings"
	"testing"
	"time"
)

func TestHumanAndEpicNeverDispatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body, title, reason string
		labels                    []string
	}{
		{name: "marker after excerpt", body: strings.Repeat("x", 600) + "\n```detent-human\nschema: 1\n```", reason: "human_owned"},
		{name: "label", labels: []string{"human-owned"}, reason: "human_owned"},
		{name: "tracking epic", title: "Epic: launch", reason: "tracking_epic"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := Normalize(Record{ID: 1, Title: tt.title, Body: tt.body, Labels: tt.labels, SourceState: "open", WorkflowState: &WorkflowState{Name: "Todo", Dispatchable: true}})
			if item.NonExecutableReason != tt.reason || item.Dispatchability.Dispatchable {
				t.Fatalf("normalized item = %+v", item)
			}
			queue := DeriveDispatchQueue([]WorkItem{item}, DispatchSnapshot{EvaluatedAt: time.Now()})
			if len(queue.Dispatchable) != 0 || len(queue.NonDispatchable) != 1 {
				t.Fatalf("queue = %+v", queue)
			}
		})
	}
}

func TestHumanRecordKeepsPullRequestEvidence(t *testing.T) {
	t.Parallel()
	databaseID := int64(7)
	at := time.Now()
	for _, status := range []SyncStatus{SyncStatusRetrying, SyncStatusStale} {
		item := Normalize(Record{ID: 1, Labels: []string{"human-owned"}, SourceState: "open", WorkflowState: &WorkflowState{Name: "Backlog"}, GitHub: GitHubIssueReference{DatabaseID: &databaseID}, SyncStatus: status, PullRequests: []PullRequestSummary{{Number: 42, GitHubNodeID: " PR_42 ", Title: " Work ", URL: " https://github.com/owner/repo/pull/42 ", Merge: MergeSummary{RefreshedAt: &at}}}})
		if item.GitHub.DatabaseID == &databaseID || *item.GitHub.DatabaseID != 7 || item.PullRequests[0].GitHubNodeID != "PR_42" || item.SyncStatus != status {
			t.Fatalf("lost record evidence: %+v", item)
		}
		if item.Dispatchability.Dispatchable {
			t.Fatal("evidence bypassed human exclusion")
		}
	}
}
