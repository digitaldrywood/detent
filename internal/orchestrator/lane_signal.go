package orchestrator

import (
	"slices"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type laneSignalState struct {
	lane     string
	external string
}

func laneSignalWarnings(state State, groups ...[]connector.Issue) []telemetry.LaneSignalWarning {
	if state.TrackerKind != workflowconfig.TrackerGitHub {
		return nil
	}
	source := strings.TrimSpace(state.TrackerStatusSource)
	if source != workflowconfig.GitHubStatusSourceProjectV2 &&
		source != workflowconfig.GitHubStatusSourceIssueField &&
		source != workflowconfig.GitHubStatusSourceLabel {
		return nil
	}

	states := configuredLaneSignalStates(state.LaneSignalStates, state.TrackerStateMap)
	if len(states) == 0 {
		return nil
	}
	warnings := []telemetry.LaneSignalWarning{}
	seen := map[string]struct{}{}
	for _, issues := range groups {
		for _, issue := range issues {
			if issue.Closed {
				continue
			}
			var candidates []telemetry.LaneSignalWarning
			if source == workflowconfig.GitHubStatusSourceLabel {
				candidates = ignoredStatusWarnings(state, issue, states)
			} else {
				candidates = ignoredLabelWarnings(state, issue, states)
			}
			for _, warning := range candidates {
				key := laneSignalWarningKey(warning)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				warnings = append(warnings, warning)
			}
		}
	}
	slices.SortFunc(warnings, func(left, right telemetry.LaneSignalWarning) int {
		if comparison := strings.Compare(laneSignalIssueKey(left), laneSignalIssueKey(right)); comparison != 0 {
			return comparison
		}
		return strings.Compare(strings.ToLower(left.Signal), strings.ToLower(right.Signal))
	})
	return warnings
}

func configuredLaneSignalStates(lanes []string, stateMap map[string]string) map[string]laneSignalState {
	states := make(map[string]laneSignalState, len(lanes)*2)
	for _, lane := range lanes {
		lane = strings.TrimSpace(lane)
		if lane == "" {
			continue
		}
		external := mappedLaneSignalState(lane, stateMap)
		state := laneSignalState{lane: lane, external: external}
		states[strings.ToLower(lane)] = state
		states[strings.ToLower(external)] = state
	}
	return states
}

func mappedLaneSignalState(lane string, stateMap map[string]string) string {
	if external := strings.TrimSpace(stateMap[lane]); external != "" {
		return external
	}
	for configured, external := range stateMap {
		if strings.EqualFold(strings.TrimSpace(configured), lane) {
			if external = strings.TrimSpace(external); external != "" {
				return external
			}
		}
	}
	return lane
}

func ignoredLabelWarnings(state State, issue connector.Issue, states map[string]laneSignalState) []telemetry.LaneSignalWarning {
	prefix := strings.TrimSpace(state.TrackerStatusLabelPrefix)
	if prefix == "" {
		prefix = "detent:"
	}
	warnings := []telemetry.LaneSignalWarning{}
	for _, label := range issue.Labels {
		label = strings.TrimSpace(label)
		if !strings.HasPrefix(strings.ToLower(label), strings.ToLower(prefix)) {
			continue
		}
		suffix := strings.TrimSpace(label[len(prefix):])
		var matched laneSignalState
		for _, candidate := range states {
			if laneSignalSlug(candidate.external) == strings.ToLower(suffix) {
				matched = candidate
				break
			}
		}
		if matched.lane == "" {
			continue
		}
		action := "set " + laneSignalStatusField(state) + " to " + matched.external
		reason := "this project reads lanes from " + laneSignalSourceDescription(state) + "; label " + label + " has no effect; " + action
		warnings = append(warnings, newLaneSignalWarning(state, issue, "label", label, matched.lane, reason, action))
	}
	return warnings
}

func ignoredStatusWarnings(state State, issue connector.Issue, states map[string]laneSignalState) []telemetry.LaneSignalWarning {
	field := laneSignalStatusField(state)
	value := laneSignalFieldValue(issue.Fields, field)
	matched, ok := states[strings.ToLower(value)]
	if !ok || value == "" {
		return nil
	}
	prefix := strings.TrimSpace(state.TrackerStatusLabelPrefix)
	if prefix == "" {
		prefix = "detent:"
	}
	label := prefix + laneSignalSlug(matched.external)
	action := "apply label " + label
	signal := field + " " + value
	reason := "this project reads lanes from " + laneSignalSourceDescription(state) + "; " + signal + " has no effect; " + action
	return []telemetry.LaneSignalWarning{newLaneSignalWarning(state, issue, "status", signal, matched.lane, reason, action)}
}

func newLaneSignalWarning(
	state State,
	issue connector.Issue,
	kind string,
	signal string,
	lane string,
	reason string,
	action string,
) telemetry.LaneSignalWarning {
	return telemetry.LaneSignalWarning{
		ReasonCode:       telemetry.LaneSignalIgnoredReasonCode,
		IssueID:          strings.TrimSpace(issue.ID),
		Identifier:       strings.TrimSpace(issue.Identifier),
		IssueURL:         strings.TrimSpace(issue.URL),
		ConfiguredSource: laneSignalSourceDescription(state),
		SignalKind:       kind,
		Signal:           signal,
		Lane:             lane,
		Reason:           reason,
		Action:           action,
	}
}

func laneSignalStatusField(state State) string {
	field := strings.TrimSpace(state.TrackerStatusField)
	if field == "" {
		return "Status"
	}
	return field
}

func laneSignalSourceDescription(state State) string {
	switch strings.TrimSpace(state.TrackerStatusSource) {
	case workflowconfig.GitHubStatusSourceProjectV2:
		return "ProjectV2 " + laneSignalStatusField(state)
	case workflowconfig.GitHubStatusSourceIssueField:
		return "issue field " + laneSignalStatusField(state)
	case workflowconfig.GitHubStatusSourceLabel:
		prefix := strings.TrimSpace(state.TrackerStatusLabelPrefix)
		if prefix == "" {
			prefix = "detent:"
		}
		return "labels with prefix " + prefix
	default:
		return strings.TrimSpace(state.TrackerStatusSource)
	}
}

func laneSignalFieldValue(fields map[string]string, field string) string {
	if value := strings.TrimSpace(fields[field]); value != "" {
		return value
	}
	for name, value := range fields {
		if strings.EqualFold(strings.TrimSpace(name), field) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func laneSignalSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	lastSeparator := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
			lastSeparator = false
		default:
			if builder.Len() == 0 || lastSeparator {
				continue
			}
			builder.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func laneSignalWarningKey(warning telemetry.LaneSignalWarning) string {
	return strings.ToLower(laneSignalIssueKey(warning) + "\x00" + warning.SignalKind + "\x00" + warning.Signal)
}

func laneSignalIssueKey(warning telemetry.LaneSignalWarning) string {
	for _, value := range []string{warning.IssueID, warning.Identifier, warning.IssueURL} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return warning.Signal
}
