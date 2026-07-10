package telemetry

import (
	"strconv"
	"strings"
)

type BoardWorkloadCounts struct {
	Load    int
	Todo    int
	Active  int
	Waiting int
	Blocked int
}

func BoardWorkload(snapshot Snapshot) BoardWorkloadCounts {
	return boardWorkload(snapshot, "")
}

func BoardWorkloadForProject(snapshot Snapshot, projectID string) BoardWorkloadCounts {
	return boardWorkload(snapshot, strings.TrimSpace(projectID))
}

type boardWorkloadIssue struct {
	state   string
	waiting bool
	rank    int
}

func boardWorkload(snapshot Snapshot, projectID string) BoardWorkloadCounts {
	issues := map[string]boardWorkloadIssue{}
	sequence := 0
	add := func(issue Issue, fallback string, rank int, waiting bool) {
		if !boardWorkloadProjectMatches(issue, snapshot.Project.ID, projectID) {
			return
		}
		state := normalizedWorkloadState(issue.State)
		if state == "" {
			state = fallback
		}
		if state == "" {
			return
		}
		key := issueStateKey(issue)
		if key == "" {
			sequence++
			key = "anonymous:" + strconv.Itoa(sequence)
		}
		current, ok := issues[key]
		if !ok || rank >= current.rank {
			issues[key] = boardWorkloadIssue{state: state, waiting: waiting || current.waiting, rank: rank}
			return
		}
		if waiting {
			current.waiting = true
			issues[key] = current
		}
	}

	for _, row := range snapshot.Completed {
		add(row.Issue, normalizedWorkloadState(row.FinalState), 5, false)
	}
	for _, row := range snapshot.Queue {
		add(row.Issue, "Todo", 10, false)
	}
	for _, row := range snapshot.Running {
		add(row.Issue, "In Progress", 10, false)
	}
	for _, row := range snapshot.Blocked {
		waiting := blockedRowDependencyWaiting(row)
		fallback := "Blocked"
		if waiting {
			fallback = "Todo"
		}
		add(row.Issue, fallback, 10, waiting)
	}
	for _, issue := range snapshot.BoardIssues {
		add(issue, "", 30, false)
	}
	for _, issue := range snapshot.Pipeline {
		add(issue, "", 40, false)
	}

	var counts BoardWorkloadCounts
	for _, issue := range issues {
		switch issue.state {
		case "Todo":
			counts.Load++
			if issue.waiting {
				counts.Waiting++
			} else {
				counts.Todo++
			}
		case "In Progress", "Rework", "Merging", "Human Review":
			counts.Load++
			counts.Active++
		case "Blocked":
			counts.Blocked++
		}
	}
	return counts
}

func boardWorkloadProjectMatches(issue Issue, fallbackProjectID string, projectID string) bool {
	if projectID == "" {
		return true
	}
	issueProjectID := strings.TrimSpace(issue.ProjectID)
	if issueProjectID == "" {
		issueProjectID = strings.TrimSpace(fallbackProjectID)
	}
	return issueProjectID == projectID
}

func normalizedWorkloadState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "todo":
		return "Todo"
	case "in progress", "in_progress":
		return "In Progress"
	case "rework":
		return "Rework"
	case "merging":
		return "Merging"
	case "human review", "human_review":
		return "Human Review"
	case "blocked":
		return "Blocked"
	}
	return ""
}

func blockedRowDependencyWaiting(row Blocked) bool {
	if strings.EqualFold(strings.TrimSpace(row.State), "Blocked") {
		return false
	}
	if row.Source == BlockedSourceDependency || strings.EqualFold(strings.TrimSpace(row.RecoveryReason), "dependency_blocker") {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(row.Error))
	return reason == "blocked by non-terminal dependency" || strings.HasPrefix(reason, "depends on ") || len(row.BlockedBy) > 0
}
