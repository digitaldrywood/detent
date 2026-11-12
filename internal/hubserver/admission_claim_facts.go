package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

func eventClaimFacts(run *tracker.NativeRunData) []any {
	var data tracker.NativeRunData
	if run != nil {
		data = *run
	}
	var checkpoint tracker.NativeCheckpoint
	if data.Handoff != nil {
		checkpoint = *data.Handoff
	}
	var change tracker.NativeChangeReference
	if checkpoint.Change != nil {
		change = *checkpoint.Change
	}
	return []any{data.AttemptID, data.RunID, data.LeaseID, data.FencingToken, data.Sequence, data.PolicyID, change.ChangeID, change.VersionID, change.HeadSHA, checkpoint.ExternalEffect, checkpoint.EffectState}
}

func answeredClaimKind(data tracker.NativeRunData, checkpoint *tracker.NativeCheckpoint) string {
	disposition := data.Disposition
	publication := checkpoint != nil && (checkpoint.ExternalEffect == "git_push" || checkpoint.ExternalEffect == "pr_create")
	if publication && (disposition == nil || checkpoint.EffectState == "pending" || checkpoint.EffectState == "ambiguous") {
		return ""
	}
	if disposition != nil {
		if disposition.Status == "in_progress" && !disposition.Blockers && !disposition.HumanAction && disposition.ReasonCode == "" {
			return ""
		}
		if disposition.HumanAction {
			return "human_action"
		}
		if disposition.ReasonCode == "permission_wait" {
			return "permission_wait"
		}
	}
	if data.Runtime != nil && data.Runtime.Completion != nil && data.Runtime.Completion.AcceptanceRecorded {
		return "recorded_acceptance"
	}
	return ""
}
