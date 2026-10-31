package orchestrator

import (
	"context"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) LocalIntakeEnabled() bool {
	return o != nil && !o.localIntakeDisabled.Load()
}

func (o *Orchestrator) setLocalIntakeDisabled(disabled bool) {
	o.dispatchStartMu.Lock()
	changed := o.localIntakeDisabled.Swap(disabled) != disabled
	o.dispatchStartMu.Unlock()
	if changed && o.logger != nil {
		o.logger.Info("local intake policy applied", "project_id", o.projectID, "enabled", !disabled)
	}
}

func (o *Orchestrator) localIntakeAllows(state *State, issue connector.Issue) bool {
	if o.LocalIntakeEnabled() {
		return true
	}
	if state == nil {
		return false
	}
	_, admitted := state.localAdmitted[issue.ID]
	_, running := state.Running[issue.ID]
	_, deferred := state.deferredCompletions[issue.ID]
	return admitted || running || deferred
}

func (o *Orchestrator) localIntakeIssues(state *State, issues []connector.Issue) []connector.Issue {
	if o.LocalIntakeEnabled() {
		return issues
	}
	return slices.DeleteFunc(slices.Clone(issues), func(issue connector.Issue) bool {
		return !o.localIntakeAllows(state, issue)
	})
}

func (o *Orchestrator) recoverLocalAdmissions(ctx context.Context, state *State) {
	reader, ok := o.workAttempts.(store.LocalAdmissionStore)
	if !ok || state == nil || state.localAdmissionRecovered || strings.TrimSpace(o.projectID) == "" {
		return
	}
	ids, err := reader.ListLocalAdmittedIssueIDs(ctx, o.projectID)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("local admitted cohort lookup failed", "project_id", o.projectID, "error", err)
		}
		return
	}
	if state.localAdmitted == nil {
		state.localAdmitted = make(map[string]struct{}, len(ids))
	}
	state.localAdmissionRecovered = true
	for _, id := range ids {
		state.localAdmitted[id] = struct{}{}
	}
}

func (o *Orchestrator) snapshotLocalIntake(state *State) {
	status := telemetry.LocalIntake{Enabled: o.LocalIntakeEnabled(), Remaining: []string{}, Blocked: []string{}}
	issues := make(map[string]connector.Issue)
	for _, issue := range state.BoardIssues {
		issues[issue.ID] = issue
	}
	for _, issue := range state.Pipeline {
		if _, known := issues[issue.ID]; !known {
			issues[issue.ID] = issue
		}
	}
	for id, retry := range state.Retry {
		if _, known := issues[id]; !known {
			issues[id] = retry.Issue
		}
	}
	for id, running := range state.Running {
		issues[id] = running.Issue
	}
	for id, deferred := range state.deferredCompletions {
		issues[id] = deferred.Running.Issue
	}
	for id, issue := range issues {
		_, admitted := state.localAdmitted[id]
		_, running := state.Running[id]
		_, deferred := state.deferredCompletions[id]
		if !admitted && !running && !deferred || issue.Closed || stateIn(issue.State, o.cfg.TerminalStates) {
			continue
		}
		if normalizeState(issue.State) == normalizeState(blockedStatusState) {
			status.Blocked = append(status.Blocked, id)
		} else {
			status.Remaining = append(status.Remaining, id)
		}
	}
	slices.Sort(status.Remaining)
	slices.Sort(status.Blocked)
	previous := state.LocalIntake
	if o.logger != nil && (!status.Enabled || !previous.Enabled) && (previous.Enabled != status.Enabled || !slices.Equal(previous.Remaining, status.Remaining) || !slices.Equal(previous.Blocked, status.Blocked)) {
		o.logger.Info("local intake cohort observed", "project_id", o.projectID, "enabled", status.Enabled, "remaining", status.Remaining, "blocked", status.Blocked)
	}
	state.LocalIntake = status
}
