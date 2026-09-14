package telemetry

import (
	"testing"
	"time"
)

func TestIssueCardFacts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		pr   *PullRequest
		ci   string
	}{
		{name: "no PR"},
		{name: "queued", pr: &PullRequest{Checks: []PullRequestCheck{{Status: "queued"}}}, ci: "queued"},
		{name: "running beats queued", pr: &PullRequest{Checks: []PullRequestCheck{{Status: "queued"}, {Status: "in_progress"}}}, ci: "running"},
		{name: "normalized pass", pr: &PullRequest{CIStatus: "pass"}, ci: "green"},
		{name: "normalized fail", pr: &PullRequest{CIStatus: "fail", Checks: []PullRequestCheck{{Status: "in_progress"}}}, ci: "red"},
		{name: "green", pr: &PullRequest{CIStatus: "SUCCESS"}, ci: "green"},
		{name: "red required check", pr: &PullRequest{CIStatus: "FAILURE", Checks: []PullRequestCheck{{Status: "in_progress"}}}, ci: "red"},
		{name: "red conclusion", pr: &PullRequest{Checks: []PullRequestCheck{{Conclusion: "timed_out"}}}, ci: "red"},
		{name: "skipped", pr: &PullRequest{CIStatus: "SUCCESS", Checks: []PullRequestCheck{{Conclusion: "skipped"}}}, ci: "skipped"},
		{name: "unknown", pr: &PullRequest{}, ci: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := Issue{ID: "i", ProjectID: "p", PullRequest: tt.pr, LaneReason: "merge_conflict", LaneReasonAt: &at}
			facts := IssueCardFacts(Snapshot{}, issue)
			if facts.CI != tt.ci || facts.HasPR != (tt.pr != nil) || facts.SessionStartedAt != nil || facts.LaneReason != "merge_conflict" || !facts.LaneReasonAt.Equal(at) {
				t.Fatalf("facts = %+v", facts)
			}
			snapshot := Snapshot{Running: []Running{{Issue: Issue{ID: "i", ProjectID: "other"}, StartedAt: at, Tokens: Tokens{Total: 99}}, {Issue: issue, StartedAt: at, Tokens: Tokens{Total: 12345}}}}
			facts = IssueCardFacts(snapshot, issue)
			if facts.SessionStartedAt == nil || !facts.SessionStartedAt.Equal(at) || facts.SessionTokens != 12345 {
				t.Fatalf("running facts = %+v", facts)
			}
		})
	}
}

func TestActiveCardLane(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		lane string
		want bool
	}{{"Todo", true}, {"In Progress", true}, {"Rework", true}, {"Merging", true}, {"Done", false}, {"Backlog", false}, {"Blocked", false}} {
		t.Run(tt.lane, func(t *testing.T) {
			if got := ActiveCardLane(tt.lane); got != tt.want {
				t.Fatalf("ActiveCardLane = %v", got)
			}
		})
	}
}

func TestCardIssuesRuntimePrecedence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, board, runtime, want string }{{"runtime-only raw", "", "OPEN", "In Progress"}, {"runtime-only empty", "", "", "In Progress"}, {"raw keeps tracker", "Rework", "OPEN", "Rework"}, {"running takes precedence", "Todo", "Rework", "Rework"}} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := Snapshot{Project: Project{ID: "p"}, Running: []Running{{Issue: Issue{ID: "i", State: tt.runtime, PullRequest: &PullRequest{HeadSHA: "new"}}}}}
			if tt.board != "" {
				snapshot.BoardIssues = []Issue{{ID: "i", State: tt.board, PullRequest: &PullRequest{HeadSHA: "old"}}}
			}
			cards := CardIssues(snapshot)
			if len(cards) != 1 || cards[0].State != tt.want || cards[0].ProjectID != "p" {
				t.Fatalf("cards = %+v", cards)
			}
			wantHead := "new"
			if tt.name == "raw keeps tracker" {
				wantHead = "old"
			}
			if cards[0].PullRequest.HeadSHA != wantHead {
				t.Fatalf("head = %s", cards[0].PullRequest.HeadSHA)
			}
		})
	}
}

func TestIssueCardFactsSessionLiveness(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name            string
		lastKnown, want bool
	}{{"live", false, true}, {"last known", true, false}} {
		t.Run(tt.name, func(t *testing.T) {
			issue := Issue{ID: "i", ProjectID: "p", PullRequest: &PullRequest{HeadSHA: "head"}}
			snapshot := Snapshot{LastKnown: tt.lastKnown, Running: []Running{{Issue: issue, StartedAt: time.Now(), Tokens: Tokens{Total: 123}}}}
			got := IssueCardFacts(snapshot, issue)
			if got.HasSession != tt.want || (got.SessionStartedAt != nil) != tt.want || got.HeadSHA != "head" {
				t.Fatalf("facts = %+v", got)
			}
			if !tt.want && got.SessionTokens != 0 {
				t.Fatalf("last-known tokens = %d", got.SessionTokens)
			}
		})
	}
}
