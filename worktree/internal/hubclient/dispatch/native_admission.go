package dispatch

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeAdmissionSource struct {
	scheduler *Scheduler
	source    *NativeConnector
}

func (s nativeAdmissionSource) Recovery(ctx context.Context, item tracker.NativeWorkItemID) (tracker.NativeRecovery, error) {
	return s.source.NativeClient().Recovery(ctx, item)
}

func (s nativeAdmissionSource) Release(ctx context.Context, lease tracker.NativeLease, reason string) error {
	return s.source.NativeClient().Release(ctx, lease, reason)
}

func (s nativeAdmissionSource) Forget(lease tracker.NativeLease) {
	s.source.NativeClient().Transport().ForgetLease(s.source.NativeClient().Scope(), lease)
}

func (s nativeAdmissionSource) Intake(ctx context.Context, lease tracker.NativeLease, issue tracker.NativeIssue) error {
	return s.scheduler.intakeNativeSource(ctx, s.source, lease, issue)
}
