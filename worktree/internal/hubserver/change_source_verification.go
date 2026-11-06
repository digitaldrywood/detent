package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (s *Service) verifyChangeSource(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	select {
	case s.sourceVerification <- struct{}{}:
		defer func() { <-s.sourceVerification }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := workspace.VerifyChangeSource(ctx, source, bundle); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nativeRejection(nativeInvalid(err.Error()), "source_verification", "source")
	}
	return nil
}
