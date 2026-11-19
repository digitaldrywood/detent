package claimapi

import (
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
)

func NativeClaimAnswerRequired(plan hubrecords.NativeClaimPlan) bool {
	return !hubapi.StringsEqualFold(plan.Candidate.State, "Rework") && plan.LatestAttemptFound && plan.LatestAttempt.Status == "succeeded" && plan.LatestAttempt.WorkItemRevision >= plan.Item.Revision && plan.LatestAttempt.DispatchGeneration >= plan.Item.DispatchGeneration && !plan.LandingPending && !hubapi.StringsEqualFold(plan.Candidate.State, "Merging")
}

func NativeClaimDispatchSatisfied(plan hubrecords.NativeClaimPlan) bool {
	if !NativeClaimAnswerRequired(plan) || plan.LatestAttempt.ClaimDisposition == nil || *plan.LatestAttempt.ClaimDisposition == "progress" {
		return false
	}
	return *plan.LatestAttempt.ClaimDisposition != "" || plan.Item.DispatchRequestedAt != nil
}
