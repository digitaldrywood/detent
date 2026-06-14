package telemetry

import (
	"sort"
	"strings"
	"time"
)

type BoardStateCount struct {
	State string `json:"state"`
	Count int    `json:"count"`
}

type BoardProgressPoint struct {
	At    time.Time `json:"at"`
	Label string    `json:"label"`
	Count int       `json:"count"`
}

var primaryBoardStateOrder = []string{
	"Todo",
	"In Progress",
	"Review",
	"Merging",
	"Done",
}

var secondaryBoardStateOrder = []string{
	"Backlog",
	"Rework",
	"Blocked",
}

func BoardStateCounts(snapshot Snapshot) []BoardStateCount {
	counts := map[string]int{}
	issues := map[string]issueStateCount{}
	addStateCount := func(state string, fallback string, count int) {
		if count <= 0 {
			return
		}
		state = normalizeBoardState(state)
		if state == "" {
			state = fallback
		}
		if state == "" {
			return
		}
		counts[state] += count
	}
	addIssueState := func(issue Issue, fallback string, rank int, preferFallback bool) {
		state := fallback
		if !preferFallback {
			state = normalizeBoardState(issue.State)
			if state == "" {
				state = fallback
			}
		}
		if state == "" {
			return
		}
		key := issueStateKey(issue)
		if key == "" {
			counts[state]++
			return
		}
		current, ok := issues[key]
		if !ok || rank >= current.rank {
			issues[key] = issueStateCount{state: state, rank: rank}
		}
	}

	for _, issue := range snapshot.Pipeline {
		addIssueState(issue, "", 10, false)
	}
	for _, row := range snapshot.Queue {
		addIssueState(row.Issue, "Todo", 20, false)
	}
	addStateCount("", "Todo", aggregateDelta(snapshot.Counts.Queue, len(snapshot.Queue)))
	for _, row := range snapshot.Running {
		addIssueState(row.Issue, "In Progress", 30, false)
	}
	addStateCount("", "In Progress", aggregateDelta(snapshot.Counts.Running, len(snapshot.Running)))
	for _, row := range snapshot.Blocked {
		addIssueState(row.Issue, "Blocked", 40, true)
	}
	addStateCount("", "Blocked", aggregateDelta(snapshot.Counts.Blocked, len(snapshot.Blocked)))

	for _, issue := range issues {
		counts[issue.state]++
	}

	return orderedBoardStateCounts(counts)
}

type issueStateCount struct {
	state string
	rank  int
}

func issueStateKey(issue Issue) string {
	scope := strings.TrimSpace(issue.ProjectID)
	prefix := ""
	if scope != "" {
		prefix = "project:" + scope + ":"
	}
	if id := strings.TrimSpace(issue.ID); id != "" {
		return prefix + "id:" + id
	}
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return prefix + "identifier:" + identifier
	}
	return ""
}

func BoardProgressPoints(snapshot Snapshot) []BoardProgressPoint {
	completed := make([]Completed, 0, len(snapshot.Completed))
	for _, row := range snapshot.Completed {
		if !row.CompletedAt.IsZero() {
			completed = append(completed, row)
		}
	}
	if len(completed) == 0 {
		if snapshot.Counts.Completed <= 0 || snapshot.GeneratedAt.IsZero() {
			return nil
		}
		at := snapshot.GeneratedAt.UTC()
		return []BoardProgressPoint{{At: at, Label: at.Format("15:04"), Count: snapshot.Counts.Completed}}
	}

	sort.SliceStable(completed, func(i, j int) bool {
		left := completed[i].CompletedAt.UTC()
		right := completed[j].CompletedAt.UTC()
		if !left.Equal(right) {
			return left.Before(right)
		}
		return completed[i].ID < completed[j].ID
	})

	offset := aggregateDelta(snapshot.Counts.Completed, len(completed))
	points := make([]BoardProgressPoint, 0, len(completed))
	for i, row := range completed {
		at := row.CompletedAt.UTC()
		points = append(points, BoardProgressPoint{
			At:    at,
			Label: at.Format("15:04"),
			Count: offset + i + 1,
		})
	}
	return points
}

func aggregateDelta(total int, details int) int {
	if total <= details {
		return 0
	}
	return total - details
}

func orderedBoardStateCounts(counts map[string]int) []BoardStateCount {
	total := 0
	for _, count := range counts {
		total += count
	}
	if total == 0 {
		return nil
	}

	out := make([]BoardStateCount, 0, len(counts))
	for _, state := range primaryBoardStateOrder {
		count := counts[state]
		if count > 0 {
			out = append(out, BoardStateCount{State: state, Count: count})
		}
		delete(counts, state)
	}
	for _, state := range secondaryBoardStateOrder {
		count := counts[state]
		if count > 0 {
			out = append(out, BoardStateCount{State: state, Count: count})
		}
		delete(counts, state)
	}

	extras := make([]string, 0, len(counts))
	for state, count := range counts {
		if count > 0 {
			extras = append(extras, state)
		}
	}
	sort.Strings(extras)
	for _, state := range extras {
		out = append(out, BoardStateCount{State: state, Count: counts[state]})
	}
	return out
}

func normalizeBoardState(state string) string {
	state = strings.Join(strings.Fields(strings.TrimSpace(state)), " ")
	switch strings.ToLower(strings.ReplaceAll(state, " ", "")) {
	case "":
		return ""
	case "todo":
		return "Todo"
	case "inprogress", "running":
		return "In Progress"
	case "review", "humanreview", "inreview":
		return "Review"
	case "merging":
		return "Merging"
	case "done", "complete", "completed", "closed", "cancelled", "canceled":
		return "Done"
	case "backlog":
		return "Backlog"
	case "rework":
		return "Rework"
	case "blocked":
		return "Blocked"
	default:
		return state
	}
}
