package orchestrator

import (
	"strings"
)

type laneSignalState struct {
	lane     string
	external string
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
