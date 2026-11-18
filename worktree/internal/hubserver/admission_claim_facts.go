package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
