package hubserver

func nativeClaimDispatchSatisfied(plan nativeClaimPlan) bool {
	return plan.latestAttemptFound && plan.latestAttempt.Status == "succeeded" && plan.latestAttempt.WorkItemRevision >= plan.item.Revision && plan.latestAttempt.DispatchGeneration >= plan.item.DispatchGeneration && !plan.landingPending && !stringsEqualFold(plan.candidate.State, "Merging")
}
