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
	return boardWorkload(snapshot, "", !boardWorkloadUsesCompleteProjection(snapshot, ""))
}

func BoardWorkloadForProject(snapshot Snapshot, projectID string) BoardWorkloadCounts {
	projectID = strings.TrimSpace(projectID)
	return boardWorkload(snapshot, projectID, !boardWorkloadUsesCompleteProjection(snapshot, projectID))
}

func CurrentBoardWorkload(snapshot Snapshot) BoardWorkloadCounts {
	return boardWorkload(snapshot, "", false)
}

func CurrentBoardWorkloadForProject(snapshot Snapshot, projectID string) BoardWorkloadCounts {
	return boardWorkload(snapshot, strings.TrimSpace(projectID), false)
}

func BoardWorkloadComplete(snapshot Snapshot) bool {
	return boardWorkloadComplete(snapshot, "")
}

func BoardWorkloadCompleteForProject(snapshot Snapshot, projectID string) bool {
	return boardWorkloadComplete(snapshot, strings.TrimSpace(projectID))
}

type boardWorkloadIssue struct {
	state   string
	waiting bool
	rank    int
}

func boardWorkload(snapshot Snapshot, projectID string, includeCompleted bool) BoardWorkloadCounts {
	issues := map[string]boardWorkloadIssue{}
	sequence := 0
	add := func(issue Issue, fallback string, rank int, waiting bool) {
		if !boardWorkloadProjectMatches(issue, snapshot.Project.ID, projectID) {
			return
		}
		boardState := normalizeBoardState(strings.ReplaceAll(issue.State, "_", " "))
		state := normalizedWorkloadState(boardState)
		if state == "" {
			state = fallback
		}
		if state == "" {
			if rank < 30 || !nonWorkloadBoardState(boardState) {
				return
			}
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

	if includeCompleted {
		for _, row := range snapshot.Completed {
			add(row.Issue, normalizedWorkloadState(row.FinalState), 5, false)
		}
	}
	for _, row := range snapshot.Queue {
		add(row.Issue, "Todo", 10, false)
	}
	for _, row := range snapshot.Running {
		add(row.Issue, "In Progress", 10, false)
	}
	for _, row := range snapshot.Blocked {
		waiting := BlockedRowDependencyWaiting(row)
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
	for _, row := range snapshot.Blocked {
		if BlockedRowDependencyWaiting(row) || !boardWorkloadProjectMatches(row.Issue, snapshot.Project.ID, projectID) {
			continue
		}
		key := issueStateKey(row.Issue)
		current, ok := issues[key]
		if !ok || current.state == "" {
			continue
		}
		current.state = "Blocked"
		current.waiting = false
		issues[key] = current
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

func boardWorkloadComplete(snapshot Snapshot, projectID string) bool {
	tracker, runtime, declared := boardWorkloadSections(snapshot, projectID)
	if !declared {
		return true
	}
	return tracker.Available() && tracker.Complete && runtime.Available() && runtime.Complete
}

func boardWorkloadUsesCompleteProjection(snapshot Snapshot, projectID string) bool {
	_, _, declared := boardWorkloadSections(snapshot, projectID)
	return declared && boardWorkloadComplete(snapshot, projectID)
}

func boardWorkloadSections(snapshot Snapshot, projectID string) (SnapshotSection, SnapshotSection, bool) {
	if projectID != "" {
		for _, project := range snapshot.Projects {
			if strings.TrimSpace(project.Project.ID) == projectID {
				return project.Tracker, project.Runtime, !project.Tracker.IsZero() || !project.Runtime.IsZero()
			}
		}
	}
	return snapshot.Tracker, snapshot.Runtime, !snapshot.Tracker.IsZero() || !snapshot.Runtime.IsZero()
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
	switch normalizeBoardState(strings.ReplaceAll(state, "_", " ")) {
	case "Todo":
		return "Todo"
	case "In Progress":
		return "In Progress"
	case "Rework":
		return "Rework"
	case "Merging":
		return "Merging"
	case "Review":
		return "Human Review"
	case "Blocked":
		return "Blocked"
	}
	return ""
}

func nonWorkloadBoardState(state string) bool {
	switch state {
	case "Backlog", "Done":
		return true
	}
	return false
}

func BlockedRowDependencyWaiting(row Blocked) bool {
	if strings.EqualFold(strings.TrimSpace(row.State), "Blocked") {
		return false
	}
	hasUnresolvedRef := false
	for _, ref := range row.BlockedBy {
		trackerState := strings.ToLower(strings.TrimSpace(ref.TrackerState))
		if trackerState == "closed" {
			continue
		}
		state := normalizeBoardState(strings.ReplaceAll(ref.State, "_", " "))
		if trackerState != "open" && (state == "Done" || state == "Cancelled" || state == "Canceled" || state == "Closed") {
			continue
		}
		hasUnresolvedRef = true
		break
	}
	if row.Source == BlockedSourceDependency && (hasUnresolvedRef || len(row.BlockedBy) == 0) {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(row.Error))
	return reason == "blocked by non-terminal dependency" || strings.HasPrefix(reason, "depends on ") || hasUnresolvedRef
}
