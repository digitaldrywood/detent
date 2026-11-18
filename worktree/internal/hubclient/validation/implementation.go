package validation

import (
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ImplementationReviewRequired(version tracker.ChangeVersion) bool {
	return version.Policy.Gates.AutomatedReview == gate.AutomatedReviewRequired
}
func NeedsImplementationReview(version tracker.ChangeVersion, reviews []tracker.ChangeReview, discussion runner.NativeReviewDiscussion) bool {
	if !ImplementationReviewRequired(version) {
		return false
	}
	var latest *tracker.ChangeReview
	for i := range reviews {
		review := &reviews[i]
		if review.VersionID == version.ID && review.ImplementationReview != nil && review.ImplementationReview.HeadSHA == version.HeadSHA && (latest == nil || !review.CreatedAt.Before(latest.CreatedAt)) {
			latest = review
		}
	}
	return latest == nil || latest.Decision != "approved" || discussion.HumanFeedbackSince(latest.CreatedAt)
}
