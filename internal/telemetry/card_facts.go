package telemetry

import (
	"strings"
	"time"
)

// CardFacts is the shared board/API projection. Nil values mean evidence is unavailable.
type CardFacts struct {
	HasSession       bool       `json:"has_session"`
	HasPR            bool       `json:"has_pr"`
	HeadSHA          string     `json:"head_sha"`
	HeadCommittedAt  *time.Time `json:"head_committed_at"`
	CI               string     `json:"ci"`
	Mergeability     string     `json:"mergeability"`
	SessionStartedAt *time.Time `json:"session_started_at"`
	SessionTokens    int64      `json:"session_tokens"`
	AttemptsToday    *int64     `json:"attempts_today"`
	LaneReason       string     `json:"lane_reason"`
	LaneReasonAt     *time.Time `json:"lane_reason_at"`
}

func ActiveCardLane(state string, configured ...string) bool {
	if len(configured) > 0 {
		for _, lane := range configured {
			if strings.EqualFold(strings.TrimSpace(state), strings.TrimSpace(lane)) {
				return true
			}
		}
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "todo", "in progress", "rework", "merging":
		return true
	default:
		return false
	}
}

func IssueCardFacts(snapshot Snapshot, issue Issue) CardFacts {
	facts := CardFacts{AttemptsToday: issue.AttemptsToday, LaneReason: issue.LaneReason, LaneReasonAt: issue.LaneReasonAt}
	if pr := issue.PullRequest; pr != nil {
		facts.HasPR = true
		facts.HeadSHA = pr.HeadSHA
		facts.HeadCommittedAt = pr.HeadCommittedAt
		facts.Mergeability = strings.ToLower(pr.MergeableState)
		facts.CI = cardCI(pr)
	}
	if snapshot.LastKnown {
		return facts
	}
	for _, running := range snapshot.Running {
		if (running.ProjectID == issue.ProjectID || running.ProjectID == "" && snapshot.Project.ID == issue.ProjectID) && cardSessionMatches(running.Issue, issue) {
			facts.HasSession = true
			if !running.StartedAt.IsZero() {
				at := running.StartedAt
				facts.SessionStartedAt = &at
			}
			facts.SessionTokens = running.Tokens.Total
			break
		}
	}
	return facts
}

func cardCI(pr *PullRequest) string {
	switch strings.ToLower(pr.CIStatus) {
	case "failure", "fail", "failed", "error", "red":
		return "red"
	}
	queued, running, green, skipped := false, false, false, false
	for _, check := range pr.Checks {
		switch strings.ToLower(check.Status) {
		case "queued", "waiting", "requested", "pending":
			queued = true
		case "in_progress", "running":
			running = true
		}
		switch strings.ToLower(check.Conclusion) {
		case "failure", "error", "timed_out", "cancelled", "action_required", "startup_failure":
			return "red"
		case "success":
			green = true
		case "skipped", "neutral":
			skipped = true
		}
	}
	if running || len(pr.RunningChecks) > 0 {
		return "running"
	}
	if queued {
		return "queued"
	}
	if green {
		return "green"
	}
	if skipped {
		return "skipped"
	}
	switch strings.ToLower(pr.CIStatus) {
	case "success", "pass", "passed", "green":
		return "green"
	case "pending", "expected", "queued":
		return "queued"
	case "skipped", "neutral":
		return "skipped"
	default:
		return "unknown"
	}
}

// CardIssues follows the board's pipeline, queue, running, and blocked precedence.
// Raw GitHub states only supply a fallback when no tracker lane is available.
func CardIssues(snapshot Snapshot) []Issue {
	result := []Issue{}
	indexes := map[string]int{}
	add := func(issue Issue, fallback string) {
		raw := strings.EqualFold(issue.State, "open") || strings.EqualFold(issue.State, "closed")
		issue.State = CardRuntimeLane(issue.State, fallback)
		if issue.State == "" {
			return
		}
		if issue.ProjectID == "" {
			issue.ProjectID = snapshot.Project.ID
		}
		key := issue.ProjectID + "\x00" + issue.ID
		if issue.ID == "" {
			key = issue.ProjectID + "\x00" + issue.Identifier
		}
		if index, found := indexes[key]; found {
			if raw {
				return
			}
			previous := result[index]
			if issue.AttemptsToday == nil {
				issue.AttemptsToday = previous.AttemptsToday
				issue.LaneReason = previous.LaneReason
				issue.LaneReasonAt = previous.LaneReasonAt
			}
			if issue.PullRequest == nil {
				issue.PullRequest = previous.PullRequest
			}
			result[index] = issue
			return
		}
		indexes[key] = len(result)
		result = append(result, issue)
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue, "")
	}
	for _, issue := range snapshot.Pipeline {
		add(issue, "")
	}
	for _, entry := range snapshot.Queue {
		add(entry.Issue, "Todo")
	}
	for _, entry := range snapshot.Running {
		add(entry.Issue, "In Progress")
	}
	for _, entry := range snapshot.Blocked {
		issue := entry.Issue
		fallback := "Todo"
		if !BlockedRowDependencyWaiting(entry) {
			issue.State = "Blocked"
			fallback = "Blocked"
		}
		add(issue, fallback)
	}
	return result
}

// CardRuntimeLane translates legacy tracker transport states into display lanes.
func CardRuntimeLane(state, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "open", "closed":
		return fallback
	default:
		return strings.TrimSpace(state)
	}
}

func cardSessionMatches(running, issue Issue) bool {
	if running.ID != "" && issue.ID != "" {
		return running.ID == issue.ID
	}
	return running.Identifier != "" && running.Identifier == issue.Identifier
}

func ActiveIssueCard(snapshot Snapshot, issue Issue) bool {
	projectID := issue.ProjectID
	if projectID == "" {
		projectID = snapshot.Project.ID
	}
	return ActiveCardLane(issue.State, snapshot.CardActiveStates[projectID]...)
}
