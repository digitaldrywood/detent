package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func (o *Orchestrator) evaluateRelease(ctx context.Context, state *State, now time.Time) {
	if o == nil || state == nil || o.release == nil {
		return
	}
	status, decision := o.release.Evaluate(ctx, now)
	state.Release = status
	if strings.TrimSpace(decision.Action) == "" {
		return
	}
	record := store.SchedulerDecision{
		ProjectID:  strings.TrimSpace(o.cfg.Project.ID),
		Lane:       "release",
		Result:     store.SchedulerDecisionResultSkipped,
		Reason:     "auto-release " + decision.Action + ": " + strings.TrimSpace(decision.Reason),
		Selected:   decision.Selected,
		DecisionAt: now,
		WaitReason: strings.TrimSpace(decision.Action),
	}
	if decision.Selected {
		record.Result = store.SchedulerDecisionResultSelected
	}
	snapshot := telemetry.SchedulerDecision{
		ProjectID:  record.ProjectID,
		Repo:       record.Repo,
		Lane:       record.Lane,
		Result:     string(record.Result),
		Reason:     record.Reason,
		Selected:   record.Selected,
		DecisionAt: record.DecisionAt,
		WaitReason: record.WaitReason,
	}
	if o.workAttempts != nil {
		id, err := o.workAttempts.RecordSchedulerDecision(ctx, record)
		if err != nil {
			o.logger.Warn("record auto-release decision failed", "action", decision.Action, "error", err)
		} else {
			snapshot.ID = id
		}
	}
	appendSchedulerDecisionSnapshot(state, snapshot)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "auto_release_" + decision.Action,
		Message: strings.TrimSpace(decision.Reason),
	})
}
