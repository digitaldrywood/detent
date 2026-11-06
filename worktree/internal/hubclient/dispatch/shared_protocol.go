package dispatch

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeLeaseLost(err error) bool { return api.NativeLeaseLost(err) }

func cancelNativeReleasedAttempt(ctx context.Context, client *NativeClient, item tracker.NativeWorkItemID, attempt tracker.NativeAttempt) error {
	return api.CancelNativeReleasedAttempt(ctx, client, item, attempt)
}

func nativeAuthorityLost(err error) bool { return api.NativeAuthorityLost(err) }
func nativeLeaseDeadline(started time.Time, lease tracker.NativeLease) time.Time {
	return api.NativeLeaseDeadline(started, lease)
}
func nativeTrackerLease(lease tracker.NativeLease) tracker.Lease {
	return api.NativeTrackerLease(lease)
}
func claimedIssue(issue connector.Issue, lease tracker.Lease) orchestrator.Claimed {
	return api.ClaimedIssue(issue, lease)
}

func diagnosticErrorCode(err error) string       { return api.DiagnosticErrorCode(err) }
func nativeAdmissionCapacityFull(err error) bool { return api.NativeAdmissionCapacityFull(err) }
