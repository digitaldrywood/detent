package evidence

import "github.com/digitaldrywood/detent/internal/tracker"

func ReviewState(recovery tracker.NativeRecovery, reviews *tracker.ChangeDetail, discussion []tracker.NativeComment) string {
	if reviews != nil {
		recovery.ChangeDetail = reviews
	}
	if discussion != nil {
		recovery.Discussion = discussion
	}
	return tracker.NativeStepReviewState(recovery)
}

func ReviewEpoch(recovery tracker.NativeRecovery, steps []tracker.NativeStepReceipt) tracker.FencingToken {
	epoch := tracker.NativeStepReviewEpoch(recovery)
	for _, receipt := range steps {
		epoch = max(epoch, receipt.ReviewEpoch)
	}
	return epoch
}
