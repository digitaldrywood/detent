package telemetry

import (
	"reflect"
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

func TestCardSessionIdentity(t *testing.T) {
	for _, tt := range []struct {
		name, id, identifier string
		want                 int64
	}{
		{"identifier only", "", "second", 22}, {"missing running ID", "id", "second", 22}, {"unknown", "", "unknown", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := Snapshot{Project: Project{ID: "p"}, Running: []Running{
				{Issue: Issue{Identifier: "first"}, Tokens: Tokens{Total: 11}}, {Issue: Issue{Identifier: "second"}, Tokens: Tokens{Total: 22}},
			}}
			got := IssueCardFacts(snapshot, Issue{ID: tt.id, Identifier: tt.identifier, ProjectID: "p"})
			if got.SessionTokens != tt.want || got.HasSession != (tt.want != 0) {
				t.Fatalf("facts = %+v", got)
			}
		})
	}
}

func TestActiveIssueCardConfiguredLanes(t *testing.T) {
	for _, tt := range []struct {
		project, lane string
		want          bool
	}{
		{"custom", "Production", true}, {"custom", "Rework", true}, {"custom", "Merging", true}, {"custom", "Done", false}, {"default", "Rework", true},
	} {
		t.Run(tt.project+tt.lane, func(t *testing.T) {
			snapshot := Snapshot{CardActiveStates: map[string][]string{"custom": {"Todo", "Production"}}}
			if got := ActiveIssueCard(snapshot, Issue{ProjectID: tt.project, State: tt.lane}); got != tt.want {
				t.Fatalf("active = %v", got)
			}
		})
	}
}

func TestCardIssuesIdentityAliases(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name                     string
		board, pipeline, running []Issue
		want                     []string
	}{
		{name: "mixed ID and identifier", board: []Issue{{ID: "id", Identifier: "DET-1", Title: "board"}}, running: []Issue{{Identifier: "DET-1", Title: "runtime"}}, want: []string{"runtime"}},
		{name: "mixed ID and URL", board: []Issue{{ID: "id", URL: "https://example.com/1", Title: "board"}}, running: []Issue{{URL: "https://example.com/1", Title: "runtime"}}, want: []string{"runtime"}},
		{name: "distinct URLs", board: []Issue{{URL: "https://example.com/1", Title: "first"}, {URL: "https://example.com/2", Title: "second"}}, want: []string{"first", "second"}},
		{name: "projects remain separate", board: []Issue{{ProjectID: "other", ID: "id", Identifier: "DET-1", URL: "url", Title: "other"}}, running: []Issue{{ID: "id", Identifier: "DET-1", URL: "url", Title: "runtime"}}, want: []string{"other", "runtime"}},
		{name: "identity fields remain separate", board: []Issue{{ID: "same", Title: "id"}, {Identifier: "same", Title: "identifier"}, {URL: "same", Title: "url"}}, want: []string{"id", "identifier", "url"}},
		{name: "missing identities remain separate", board: []Issue{{Title: "first"}, {Title: "second"}}, want: []string{"first", "second"}},
		{name: "raw copy registers aliases", board: []Issue{{ID: "id", Title: "board"}}, pipeline: []Issue{{ID: "id", Identifier: "DET-1", State: "OPEN", Title: "raw"}}, running: []Issue{{Identifier: "DET-1", Title: "runtime"}}, want: []string{"runtime"}},
		{name: "later copy bridges earlier aliases", board: []Issue{{ID: "id", Title: "board"}, {URL: "url", Title: "url"}}, running: []Issue{{ID: "id", URL: "url", Title: "runtime"}}, want: []string{"runtime"}},
		{name: "aliases survive replacement", board: []Issue{{ID: "id", Identifier: "DET-1", URL: "url", Title: "board"}}, pipeline: []Issue{{Identifier: "DET-1", Title: "pipeline"}}, running: []Issue{{URL: "url", Title: "runtime"}}, want: []string{"runtime"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := Snapshot{Project: Project{ID: "p"}, BoardIssues: tt.board, Pipeline: tt.pipeline}
			for i := range snapshot.BoardIssues {
				snapshot.BoardIssues[i].State = "Todo"
			}
			for i := range snapshot.Pipeline {
				if snapshot.Pipeline[i].State == "" {
					snapshot.Pipeline[i].State = "Rework"
				}
			}
			for _, issue := range tt.running {
				issue.State = "In Progress"
				snapshot.Running = append(snapshot.Running, Running{Issue: issue})
			}
			var got []string
			for _, issue := range CardIssues(snapshot) {
				got = append(got, issue.Title)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("card titles = %v, want %v", got, tt.want)
			}
		})
	}
}
