package github

import (
	"strconv"
	"strings"
)

func (c *Connector) detentToGitHubState(stateName string) string {
	stateName = strings.TrimSpace(stateName)
	if mapped, ok := c.stateMap[stateName]; ok {
		return strings.TrimSpace(mapped)
	}
	normalized := normalizeStateName(stateName)
	for detentState, mapped := range c.stateMap {
		if normalizeStateName(detentState) == normalized {
			return strings.TrimSpace(mapped)
		}
	}
	return stateName
}

func (c *Connector) detentToGitHubStates(stateNames []string) []string {
	states := make([]string, 0, len(stateNames))
	seen := make(map[string]struct{}, len(stateNames))
	for _, stateName := range stateNames {
		state := c.detentToGitHubState(stateName)
		key := normalizeStateName(state)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		states = append(states, state)
	}
	return states
}

func (c *Connector) projectStatusQuery(stateNames []string) string {
	states := c.detentToGitHubStates(stateNames)
	if len(states) == 0 {
		return ""
	}

	values := make([]string, 0, len(states))
	for _, state := range states {
		values = append(values, projectFilterValue(state))
	}
	return "status:" + strings.Join(values, ",")
}

func projectFilterValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '_' || r == '-' || r == '.' {
			continue
		}
		return strconv.Quote(value)
	}
	return value
}

func (c *Connector) githubToDetentState(githubState string) string {
	githubState = strings.TrimSpace(githubState)
	if githubState == "" {
		return ""
	}
	if state := c.configuredDetentState(githubState); state != "" {
		return state
	}
	for detentState, mapped := range c.stateMap {
		if normalizeStateName(mapped) == normalizeStateName(githubState) {
			return strings.TrimSpace(detentState)
		}
	}
	return githubState
}

func (c *Connector) configuredDetentState(stateName string) string {
	stateName = normalizeStateName(stateName)
	if stateName == "" {
		return ""
	}
	for _, candidate := range c.configuredStatusStates() {
		candidate = strings.TrimSpace(candidate)
		if normalizeStateName(candidate) == stateName {
			return candidate
		}
	}
	return ""
}

func (c *Connector) githubIssueStateToDetentState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "CLOSED":
		return c.closedIssueState()
	case "OPEN":
		return "Open"
	default:
		return ""
	}
}

func githubIssueClosed(state string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "CLOSED")
}

func (c *Connector) closedIssueState() string {
	for _, state := range c.terminalStates {
		if normalizeStateName(state) == "done" {
			return state
		}
	}
	for _, state := range c.terminalStates {
		if normalizeStateName(state) == "closed" {
			return state
		}
	}
	if len(c.terminalStates) > 0 {
		return c.terminalStates[0]
	}
	return "Closed"
}

func (c *Connector) priorityRank(name string) *int {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	rank, ok := c.priorityMap[name]
	if !ok || rank == nil {
		return nil
	}
	value := *rank
	return &value
}

func normalizedStateSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = normalizeStateName(state)
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

func stateSetContains(states map[string]struct{}, state string) bool {
	_, ok := states[normalizeStateName(state)]
	return ok
}

func stateListWithout(states []string, excluded string) []string {
	excluded = normalizeStateName(excluded)
	if excluded == "" {
		return normalizeStateList(states, nil)
	}
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		key := normalizeStateName(state)
		if key == "" || key == excluded {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func stateInList(state string, states []string) bool {
	normalized := normalizeStateName(state)
	if normalized == "" {
		return false
	}
	for _, candidate := range states {
		if normalized == normalizeStateName(candidate) {
			return true
		}
	}
	return false
}

func normalizeStateList(states []string, defaults []string) []string {
	if len(states) == 0 {
		states = defaults
	}
	out := make([]string, 0, len(states))
	seen := map[string]struct{}{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		key := normalizeStateName(state)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, state)
	}
	return out
}

func normalizeStateName(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func cloneStateMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "" && value != "" {
			cloned[key] = value
		}
	}
	return cloned
}

func clonePriorityMapWithDefault(values map[string]*int) map[string]*int {
	if values == nil {
		values = defaultPriorityMap()
	}
	cloned := make(map[string]*int, len(values))
	for key, value := range values {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if value == nil {
			cloned[key] = nil
			continue
		}
		rank := *value
		cloned[key] = &rank
	}
	return cloned
}

func defaultPriorityMap() map[string]*int {
	return map[string]*int{
		"Urgent":      new(1),
		"High":        new(2),
		"Medium":      new(3),
		"Low":         new(4),
		"No priority": nil,
	}
}
