package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseVerifierFixture struct {
	input tracker.ReleasePickInput
}

func (v *releaseVerifierFixture) Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error) {
	return ReconcileSnapshot{}, nil
}
func (v *releaseVerifierFixture) VerifyReleasePick(_ context.Context, input tracker.ReleasePickInput) error {
	if input != v.input {
		return ErrReleasePickMismatch
	}
	return nil
}

func (v *releaseVerifierFixture) VerifyReleasePickRefresh(ctx context.Context, _, input tracker.ReleasePickInput) error {
	return v.VerifyReleasePick(ctx, input)
}
