package runner

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
)

type sourceTurnBudgetKey struct{}

type sourceTurnBudget struct {
	remaining int
}

func sourceBudgetRemaining(ctx context.Context) int {
	if budget, ok := ctx.Value(sourceTurnBudgetKey{}).(*sourceTurnBudget); ok {
		return budget.remaining
	}
	return -1
}

func sourceBudgetExhaustedMessage(message string) string {
	return message + "\n\nThe shared implementation and review turn budget is exhausted; preserve the current source and review findings for Rework.\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
}

func nativeSourceRework(change *NativeChange, message string) string {
	disposition := NativeDispositionFromMessage(message)
	if disposition == nil || disposition.Status != "complete" || disposition.Blockers || disposition.HumanAction || disposition.ReasonCode != "" {
		return ""
	}
	if change == nil || change.Reviewed || change.VersionID == "" || change.Error != "" || change.VersionError != "" || change.ImplementationReviewError != "" || change.Landing != nil {
		return ""
	}
	var findings []string
	for _, result := range []*gate.ValidatorResult{change.ImplementationReview, change.Validator} {
		if result != nil && result.Verdict == gate.ValidatorVerdictRework && result.VersionID == change.VersionID && result.HeadSHA == change.HeadSHA {
			body, err := json.Marshal(result)
			if err == nil {
				findings = append(findings, string(body))
			}
		}
	}
	if audit := change.SecurityAudit; audit != nil && audit.Verdict == "rework" && audit.VersionID == change.VersionID && audit.Run.HeadSHA == change.HeadSHA {
		body, err := json.Marshal(audit)
		if err == nil {
			findings = append(findings, string(body))
		}
	}
	return strings.Join(findings, "\n\n")
}
