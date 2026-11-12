package dispatchplan

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	SelectionReasonStickyAged = "sticky_aged_head"
	SelectionReasonAged       = "aged_head"
	SelectionReasonClean      = "clean_head"
	SelectionReasonQueue      = "queue_order"
)

type MergeOrder struct {
	StickyIssueID string
	MergeState    string
	RepositoryKey func(connector.Issue) string
	Aged          func(connector.Issue) bool
	HeadReady     func(connector.Issue) bool
}

type MergePriority struct {
	Reasons       map[string]string
	StickyIssueID string
}

func PrioritizeReadyMergingIssues(issues []connector.Issue, order MergeOrder) MergePriority {
	priority := MergePriority{
		Reasons:       map[string]string{},
		StickyIssueID: order.StickyIssueID,
	}
	ordered := make([]connector.Issue, 0, len(issues))
	appended := make([]bool, len(issues))
	repositoryHeads := make(map[string]int)
	for index, issue := range issues {
		if !MergeWorkerIssue(issue, order.MergeState) {
			continue
		}
		repository := order.RepositoryKey(issue)
		if repository == "" {
			continue
		}
		if _, seen := repositoryHeads[repository]; !seen {
			repositoryHeads[repository] = index
		}
	}
	appendMatching := func(reason string, matches func(int, connector.Issue) bool) {
		for index, issue := range issues {
			if appended[index] || !MergeWorkerIssue(issue, order.MergeState) || !matches(index, issue) {
				continue
			}
			appended[index] = true
			ordered = append(ordered, issue)
			if issueID := strings.TrimSpace(issue.ID); issueID != "" {
				priority.Reasons[issueID] = reason
			}
		}
	}
	appendMatching(SelectionReasonStickyAged, func(_ int, issue connector.Issue) bool {
		return strings.TrimSpace(issue.ID) == priority.StickyIssueID
	})
	agedIssues := make([]connector.Issue, 0, len(issues))
	for index, issue := range issues {
		if !appended[index] && order.Aged(issue) {
			appended[index] = true
			agedIssues = append(agedIssues, issue)
		}
	}
	slices.SortStableFunc(agedIssues, func(leftIssue connector.Issue, rightIssue connector.Issue) int {
		left := MergeQueueEnteredAt(leftIssue, time.Time{})
		right := MergeQueueEnteredAt(rightIssue, time.Time{})
		if left.Before(right) {
			return -1
		}
		if left.After(right) {
			return 1
		}
		return 0
	})
	for _, issue := range agedIssues {
		ordered = append(ordered, issue)
		if issueID := strings.TrimSpace(issue.ID); issueID != "" {
			priority.Reasons[issueID] = SelectionReasonAged
		}
	}
	appendMatching(SelectionReasonClean, func(index int, issue connector.Issue) bool {
		repository := order.RepositoryKey(issue)
		if repository != "" && repositoryHeads[repository] != index {
			return false
		}
		return order.HeadReady(issue)
	})
	appendMatching(SelectionReasonQueue, func(_ int, _ connector.Issue) bool { return true })
	if len(ordered) == 0 {
		return priority
	}
	next := 0
	for index := range issues {
		if !MergeWorkerIssue(issues[index], order.MergeState) {
			continue
		}
		issues[index] = ordered[next]
		next++
	}
	return priority
}
