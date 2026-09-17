package telemetry

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
)

func TestCardQuestionEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	at := now.Add(-6 * time.Hour)
	for _, tc := range []struct {
		name, project, fingerprint, prState string
		answered, waiting                   bool
	}{
		{name: "unanswered", project: "p", waiting: true},
		{name: "same evidence", project: "p", fingerprint: "old", waiting: true},
		{name: "independent work", project: "p", fingerprint: "new"},
		{name: "closed PR", project: "p", prState: "CLOSED"},
		{name: "merged PR", project: "p", prState: "MERGED"},
		{name: "other project", project: "other"},
		{name: "answered", project: "p", answered: true},
		{name: "standalone project", waiting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := Snapshot{Project: Project{ID: "p"}, OpenQuestions: []operations.Decision{{ProjectID: "p", Issue: "owner/repo#1", Question: "Choose?", WorkFingerprint: "old", AskedAt: &at}}}
			if tc.answered {
				snapshot.OpenQuestions = nil
			}
			issue := Issue{ProjectID: tc.project, Identifier: "owner/repo#1", LaneReason: "existing", PullRequest: &PullRequest{State: tc.prState, HumanQuestionWorkFingerprint: tc.fingerprint}}
			facts := IssueCardFacts(snapshot.WithFreshness(now), issue)
			if (facts.Question != nil) != tc.waiting {
				t.Fatalf("question = %+v", facts.Question)
			}
			if tc.waiting {
				if facts.LaneReason != "waiting for a human reply" || !facts.LaneReasonAt.Equal(at) || facts.Question.AgeSeconds == nil || *facts.Question.AgeSeconds != 21600 {
					t.Fatalf("facts = %+v", facts)
				}
			} else if facts.LaneReason != "existing" {
				t.Fatalf("reason = %s", facts.LaneReason)
			}
		})
	}
}
