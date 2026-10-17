package dispatchpriority

import (
	"cmp"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

const UnmappedPriorityRank = 5

type LabelMatch struct {
	Label string
	Rank  int
}

type Ranker struct {
	stateRanks map[string]int
	labelRanks map[string]LabelMatch
}

func New(states []string, labels []string) Ranker {
	return Ranker{
		stateRanks: stateRanks(states),
		labelRanks: labelRanks(labels),
	}
}

type Candidate struct {
	Issue       connector.Issue
	ProjectRank int
	Rank        string
}

func (r Ranker) Compare(left, right Candidate, prioritizeUnblockers bool) int {
	a, b := left.Issue, right.Issue
	if order := cmp.Compare(Priority(a.Priority), Priority(b.Priority)); order != 0 {
		return order
	}
	if order := cmp.Compare(left.ProjectRank, right.ProjectRank); order != 0 {
		return order
	}
	if a.CreatedAt != nil && b.CreatedAt != nil {
		if order := a.CreatedAt.Compare(*b.CreatedAt); order != 0 {
			return order
		}
	}
	if (a.CreatedAt == nil) != (b.CreatedAt == nil) {
		if a.CreatedAt != nil {
			return -1
		}
		return 1
	}
	return cmp.Compare(a.Identifier, b.Identifier)
}

func (r Ranker) State(state string) int {
	if rank, ok := r.stateRanks[normalize(state)]; ok {
		return rank
	}
	return len(r.stateRanks)
}

func Priority(priority *int) int {
	if priority == nil || *priority < 1 || *priority >= UnmappedPriorityRank {
		return UnmappedPriorityRank
	}
	return *priority
}

func (r Ranker) Label(labels []string) int {
	match, ok := r.MatchLabel(labels)
	if !ok {
		return len(r.labelRanks)
	}
	return match.Rank
}

func (r Ranker) MatchLabel(labels []string) (LabelMatch, bool) {
	best := LabelMatch{Rank: len(r.labelRanks)}
	found := false
	for _, label := range labels {
		match, ok := r.labelRanks[normalize(label)]
		if ok && match.Rank < best.Rank {
			best = match
			found = true
		}
	}
	return best, found
}

func stateRanks(states []string) map[string]int {
	ranks := make(map[string]int, len(states))
	for _, state := range states {
		state = normalize(state)
		if state == "" {
			continue
		}
		if _, ok := ranks[state]; ok {
			continue
		}
		ranks[state] = len(ranks)
	}
	return ranks
}

func labelRanks(labels []string) map[string]LabelMatch {
	ranks := make(map[string]LabelMatch, len(labels))
	for _, label := range labels {
		display := strings.TrimSpace(label)
		key := normalize(display)
		if key == "" {
			continue
		}
		if _, ok := ranks[key]; ok {
			continue
		}
		ranks[key] = LabelMatch{Label: display, Rank: len(ranks)}
	}
	return ranks
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func AnnotateUnblockerCounts(targets []connector.Issue, issues []connector.Issue, activeStates []string, terminalStates []string, enabled bool) {
	for index := range targets {
		targets[index].UnblockerCount = 0
	}
	if !enabled || len(targets) == 0 || len(issues) == 0 {
		return
	}

	targetsByRef := make(map[string]int, len(targets)*2)
	for index, issue := range targets {
		if issue.Closed || normalize(issue.State) == "blocked" || !inStates(issue.State, activeStates) || dependencyWaiting(issue, terminalStates) {
			continue
		}
		for _, ref := range issueReferenceKeys(issue.ID, issue.Identifier) {
			targetsByRef[ref] = index
		}
	}

	counted := make(map[int]map[string]struct{})
	for _, dependent := range issues {
		if normalize(dependent.State) != "blocked" && !dependencyWaiting(dependent, terminalStates) {
			continue
		}
		dependentKey := strings.TrimSpace(dependent.ID)
		if dependentKey == "" {
			dependentKey = strings.TrimSpace(dependent.Identifier)
		}
		if dependentKey == "" {
			continue
		}
		for _, blocker := range dependent.BlockedBy {
			index, ok := unblockerTargetIndex(targetsByRef, blocker)
			if !ok || inStates(blocker.State, terminalStates) {
				continue
			}
			if counted[index] == nil {
				counted[index] = map[string]struct{}{}
			}
			if _, ok := counted[index][dependentKey]; ok {
				continue
			}
			counted[index][dependentKey] = struct{}{}
			targets[index].UnblockerCount++
		}
	}
}

func dependencyWaiting(issue connector.Issue, terminalStates []string) bool {
	for _, blocker := range issue.BlockedBy {
		if strings.TrimSpace(blocker.State) == "" || !inStates(blocker.State, terminalStates) {
			return true
		}
	}
	return false
}

func unblockerTargetIndex(targets map[string]int, blocker connector.BlockedRef) (int, bool) {
	for _, ref := range issueReferenceKeys(blocker.ID, blocker.Identifier) {
		if index, ok := targets[ref]; ok {
			return index, true
		}
	}
	return 0, false
}

func issueReferenceKeys(values ...string) []string {
	keys := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(strings.TrimSpace(value))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

func inStates(value string, states []string) bool {
	for _, state := range states {
		if normalize(value) == normalize(state) {
			return true
		}
	}
	return false
}
