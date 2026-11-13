package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/hubapi"

func nativeClaimAnswerRequired(plan nativeClaimPlan) bool {
	return !hubapi.StringsEqualFold(plan.candidate.State, "Rework") && plan.latestAttemptFound && plan.latestAttempt.Status == "succeeded" && plan.latestAttempt.WorkItemRevision >= plan.item.Revision && plan.latestAttempt.DispatchGeneration >= plan.item.DispatchGeneration && !plan.landingPending && !hubapi.StringsEqualFold(plan.candidate.State, "Merging")
}

func nativeClaimDispatchSatisfied(plan nativeClaimPlan) bool {
	if !nativeClaimAnswerRequired(plan) || plan.latestAttempt.ClaimDisposition == nil || *plan.latestAttempt.ClaimDisposition == "progress" {
		return false
	}
	return *plan.latestAttempt.ClaimDisposition != "" || plan.item.DispatchRequestedAt != nil
}
