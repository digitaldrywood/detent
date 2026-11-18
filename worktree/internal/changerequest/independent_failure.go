package changerequest

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

func independentRejections(detail tracker.ChangeDetail) []tracker.ChangeReview {
	if detail.Change.CurrentLanding() != nil || detail.Summary.Status == "stale_policy" {
		return nil
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion {
			continue
		}
		for _, expected := range version.Checks {
			for _, check := range detail.Checks {
				if check.VersionID == version.ID && check.CheckRunID == expected.CheckRunID && check.Conclusion != "success" {
					return nil
				}
			}
		}
	}
	seen := map[string]bool{}
	var rejections []tracker.ChangeReview
	for i := len(detail.Reviews) - 1; i >= 0; i-- {
		review := detail.Reviews[i]
		if review.VersionID != detail.Change.CurrentVersion {
			continue
		}
		role := "human"
		switch {
		case review.Validator != nil:
			role = "validator"
		case review.ImplementationReview != nil:
			role = "review"
		case review.SecurityAudit != nil:
			role = "audit"
		}
		if seen[role] {
			continue
		}
		seen[role] = true
		if review.Decision != "changes_requested" {
			continue
		}
		if len(review.IndependentFailures()) == 0 {
			return nil
		}
		rejections = append(rejections, review)
	}
	return rejections
}

func IndependentRepairDependencies(detail tracker.ChangeDetail) []tracker.NativeWorkItemID {
	var dependencies []tracker.NativeWorkItemID
	for _, review := range independentRejections(detail) {
		if len(review.RepairDependencies) == 0 || review.RepairRefusal != "" {
			return nil
		}
		dependencies = append(dependencies, review.RepairDependencies...)
	}
	return dependencies
}
