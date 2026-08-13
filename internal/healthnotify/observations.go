package healthnotify

import (
	"net/url"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	ScopeFleet                   = "fleet"
	ScopeProject                 = "project"
	CauseCIUnavailable           = "ci_unavailable"
	CauseDispatchStall           = "dispatch_stall"
	StateFleetNeedsAttention     = "needs_attention"
	StateProjectNeedsAttention   = "needs_human_attention"
	TransitionEntry              = "entry"
	TransitionRecovery           = "recovery"
	fleetIdentity                = "fleet"
	unknownProjectRecoveryStatus = "unknown"
)

type observation struct {
	Identity    string
	Scope       string
	ProjectID   string
	Active      bool
	State       string
	Causes      []string
	WaitReasons []string
}

func observations(snapshot telemetry.Snapshot, health []project.Health) []observation {
	projectStatuses := make(map[string]string, len(health))
	for _, current := range health {
		projectID := strings.TrimSpace(current.Project.ID)
		if projectID == "" {
			continue
		}
		projectStatuses[projectID] = strings.TrimSpace(string(current.Status))
	}
	projectCauses := map[string]map[string][]string{}
	fleetCauses := []string{}
	fleetWaitReasons := []string{}
	for _, stall := range snapshot.DispatchStalls {
		if !stall.Stalled {
			continue
		}
		projectID := strings.TrimSpace(stall.ProjectID)
		if projectID == "" {
			continue
		}
		if projectCauses[projectID] == nil {
			projectCauses[projectID] = map[string][]string{}
		}
		waitReasons := compactSorted([]string{stall.WaitReason})
		projectCauses[projectID][CauseDispatchStall] = waitReasons
		fleetCauses = append(fleetCauses, CauseDispatchStall)
		fleetWaitReasons = append(fleetWaitReasons, waitReasons...)
	}
	for _, condition := range snapshot.CIUnavailable {
		projectID := strings.TrimSpace(condition.ProjectID)
		if projectID == "" {
			continue
		}
		if projectCauses[projectID] == nil {
			projectCauses[projectID] = map[string][]string{}
		}
		projectCauses[projectID][CauseCIUnavailable] = nil
		fleetCauses = append(fleetCauses, CauseCIUnavailable)
	}

	projectIDs := make([]string, 0, len(projectStatuses)+len(projectCauses))
	seen := map[string]struct{}{}
	for projectID := range projectStatuses {
		seen[projectID] = struct{}{}
		projectIDs = append(projectIDs, projectID)
	}
	for projectID := range projectCauses {
		if _, ok := seen[projectID]; ok {
			continue
		}
		projectIDs = append(projectIDs, projectID)
	}
	slices.Sort(projectIDs)
	result := make([]observation, 0, len(projectIDs)*2+1)
	for _, projectID := range projectIDs {
		recoveryState := strings.TrimSpace(projectStatuses[projectID])
		if recoveryState == "" {
			recoveryState = unknownProjectRecoveryStatus
		}
		for _, cause := range []string{CauseCIUnavailable, CauseDispatchStall} {
			waitReasons, active := projectCauses[projectID][cause]
			state := recoveryState
			causes := []string(nil)
			if active {
				state = StateProjectNeedsAttention
				causes = []string{cause}
			}
			result = append(result, observation{
				Identity:    projectIdentity(projectID, cause),
				Scope:       ScopeProject,
				ProjectID:   projectID,
				Active:      active,
				State:       state,
				Causes:      causes,
				WaitReasons: compactSorted(waitReasons),
			})
		}
	}
	fleetState := "ok"
	if snapshot.Shutdown.Draining {
		fleetState = "draining"
	}
	fleetCauses = compactSorted(fleetCauses)
	if len(fleetCauses) > 0 {
		fleetState = StateFleetNeedsAttention
	}
	result = append(result, observation{
		Identity:    fleetIdentity,
		Scope:       ScopeFleet,
		Active:      len(fleetCauses) > 0,
		State:       fleetState,
		Causes:      fleetCauses,
		WaitReasons: compactSorted(fleetWaitReasons),
	})
	return result
}

func projectIdentity(projectID string, cause string) string {
	return "project:" + url.QueryEscape(strings.TrimSpace(projectID)) + ":" + strings.TrimSpace(cause)
}

func compactSorted(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
