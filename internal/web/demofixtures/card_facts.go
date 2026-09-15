package demofixtures

import (
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func demoCardFactsSnapshot() telemetry.Snapshot {
	snapshot := demoHealthySnapshot()
	snapshot.BoardIssues = nil
	snapshot.Pipeline = nil
	snapshot.Running = nil
	snapshot.Queue = nil
	snapshot.Blocked = nil
	for i, state := range []string{"queued", "in_progress", "success", "failure", "skipped"} {
		issue := demoIssue(demoPrimaryProjectID, "facts-"+state, "digitaldrywood/detent-core#"+strconv.Itoa(7000+i), "Card facts "+state, "Rework", i+1)
		issue.AttemptsToday = demoInt64Ptr(75)
		issue.LaneReason = "merge_conflict"
		issue.LaneReasonAt = demoTimePtr(demoBaseTime.Add(-3 * time.Hour))
		check := telemetry.PullRequestCheck{Name: "Verify", Status: "completed", Conclusion: state}
		if state == "queued" || state == "in_progress" {
			check.Status = state
			check.Conclusion = ""
		}
		issue.PullRequest = &telemetry.PullRequest{Number: 7100 + i, URL: "https://github.com/digitaldrywood/detent-core/pull/" + strconv.Itoa(7100+i), HeadSHA: "head-" + state, HeadCommittedAt: demoTimePtr(demoBaseTime.Add(-2 * time.Hour)), MergeableState: []string{"clean", "blocked", "unstable", "dirty", "clean"}[i], Checks: []telemetry.PullRequestCheck{check}}
		snapshot.BoardIssues = append(snapshot.BoardIssues, issue)
		if i == 1 {
			snapshot.Running = append(snapshot.Running, telemetry.Running{Issue: issue, StartedAt: demoBaseTime.Add(-30 * time.Minute), Tokens: telemetry.Tokens{Total: 12345}})
		}
	}
	issue := demoIssue(demoPrimaryProjectID, "facts-none", "digitaldrywood/detent-core#7005", "Card facts absent", "Todo", 6)
	issue.AttemptsToday = demoInt64Ptr(0)
	snapshot.BoardIssues = append(snapshot.BoardIssues, issue)
	return snapshot
}
