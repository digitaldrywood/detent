package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseVerifierFixture struct {
	input  tracker.ReleasePickInput
	calls  int
	refuse bool
	verify func(tracker.ReleasePickInput) error
	status func(tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error)
}

func (v *releaseVerifierFixture) ReleaseDeliveryStatus(_ context.Context, input tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error) {
	return v.status(input)
}

func (v *releaseVerifierFixture) Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error) {
	return ReconcileSnapshot{}, nil
}
func (v *releaseVerifierFixture) VerifyReleasePick(_ context.Context, input tracker.ReleasePickInput) error {
	v.calls++
	if v.refuse || input != v.input {
		return ErrReleasePickMismatch
	}
	if v.verify != nil {
		return v.verify(input)
	}
	return nil
}

func (v *releaseVerifierFixture) VerifyReleasePickRefresh(ctx context.Context, _, input tracker.ReleasePickInput) error {
	return v.VerifyReleasePick(ctx, input)
}
