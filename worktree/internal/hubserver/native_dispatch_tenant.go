package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
)

func nativeClaimAnswerRequired(plan hubrecords.NativeClaimPlan) bool {
	return !hubapi.StringsEqualFold(plan.Candidate.State, "Rework") && plan.LatestAttemptFound && plan.LatestAttempt.Status == "succeeded" && plan.LatestAttempt.WorkItemRevision >= plan.Item.Revision && plan.LatestAttempt.DispatchGeneration >= plan.Item.DispatchGeneration && !plan.LandingPending && !hubapi.StringsEqualFold(plan.Candidate.State, "Merging")
}

func nativeClaimDispatchSatisfied(plan hubrecords.NativeClaimPlan) bool {
	if !nativeClaimAnswerRequired(plan) || plan.LatestAttempt.ClaimDisposition == nil || *plan.LatestAttempt.ClaimDisposition == "progress" {
		return false
	}
	return *plan.LatestAttempt.ClaimDisposition != "" || plan.Item.DispatchRequestedAt != nil
}
