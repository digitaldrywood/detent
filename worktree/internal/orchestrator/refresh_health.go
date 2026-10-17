package orchestrator

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	refreshFailureDegradedThreshold = 3
	refreshStaleIntervalMultiplier  = 2
	defaultRefreshStaleAfter        = 2 * time.Minute
	schedulingUnavailableCondition  = "scheduling_unavailable"
)

func recordRefreshSourceSuccess(state *State, name telemetry.RefreshSourceName, at time.Time) {
	if state == nil || name == "" {
		return
	}
	if state.RefreshSources == nil {
		state.RefreshSources = map[telemetry.RefreshSourceName]telemetry.RefreshSource{}
	}
	at = at.UTC()
	source := state.RefreshSources[name]
	source.Name = name
	source.LastSuccessAt = &at
	source.Degraded = false
	source.FailureStreak = 0
	source.LastError = ""
	source.LastErrorAt = nil
	source.Condition = ""
	source.Connector = ""
	state.RefreshSources[name] = source
}

func recordRefreshSourceFailure(state *State, name telemetry.RefreshSourceName, err error, at time.Time) {
	if state == nil || name == "" {
		return
	}
	if state.RefreshSources == nil {
		state.RefreshSources = map[telemetry.RefreshSourceName]telemetry.RefreshSource{}
	}
	at = at.UTC()
	source := state.RefreshSources[name]
	source.Name = name
	source.FailureStreak++
	if err != nil {
		source.LastError = strings.TrimSpace(err.Error())
		if errors.Is(err, ErrSchedulingUnavailable) {
			source.Condition = schedulingUnavailableCondition
			source.Connector = "hub"
		} else if availabilityErr, ok := connector.AsTrackerAvailability(err); ok {
			source.Condition = connector.TrackerUnavailableCondition
			source.Connector = availabilityErr.Scope.Connector
		} else {
			source.Condition = ""
			source.Connector = ""
		}
	}
	source.LastErrorAt = &at
	state.RefreshSources[name] = source
}

func refreshStaleAfter(pollInterval time.Duration) time.Duration {
	if pollInterval <= 0 {
		return defaultRefreshStaleAfter
	}
	return refreshStaleIntervalMultiplier * pollInterval
}

func refreshSourceSnapshots(sources map[telemetry.RefreshSourceName]telemetry.RefreshSource) []telemetry.RefreshSource {
	if len(sources) == 0 {
		return nil
	}
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, string(name))
	}
	sort.Strings(names)
	out := make([]telemetry.RefreshSource, 0, len(names))
	for _, name := range names {
		source := sources[telemetry.RefreshSourceName(name)]
		source.LastSuccessAt = cloneTimePointer(source.LastSuccessAt)
		source.LastErrorAt = cloneTimePointer(source.LastErrorAt)
		out = append(out, source)
	}
	return out
}

func degradedRefreshSource(sources []telemetry.RefreshSource, threshold int, now time.Time, staleAfter time.Duration) (telemetry.RefreshSource, bool) {
	for _, source := range sources {
		if source.Degraded {
			return source, true
		}
		if source.FailureStreak >= threshold {
			return source, true
		}
		if source.LastSuccessAt != nil && staleAfter > 0 && now.After(source.LastSuccessAt.Add(staleAfter)) {
			return source, true
		}
	}
	return telemetry.RefreshSource{}, false
}

func cloneRefreshSources(sources map[telemetry.RefreshSourceName]telemetry.RefreshSource) map[telemetry.RefreshSourceName]telemetry.RefreshSource {
	if sources == nil {
		return nil
	}
	cloned := make(map[telemetry.RefreshSourceName]telemetry.RefreshSource, len(sources))
	for name, source := range sources {
		source.LastSuccessAt = cloneTimePointer(source.LastSuccessAt)
		source.LastErrorAt = cloneTimePointer(source.LastErrorAt)
		cloned[name] = source
	}
	return cloned
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
