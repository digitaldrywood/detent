package dispatchpriority

import "strings"

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
