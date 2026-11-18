package changesource

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type Retention struct{ Capture *tracker.ChangeSourceCapture }

func Retain(ctx context.Context, required bool, current *tracker.ChangeSourceCapture, diff tracker.AttemptDiffRequest, captureSource func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) (Retention, error) {
	if !required || diff.HeadSHA == diff.BaseSHA {
		return Retention{Capture: current}, nil
	}
	if current != nil && current.Source.BaseSHA == diff.BaseSHA && current.Source.HeadSHA == diff.HeadSHA {
		return Retention{Capture: current}, current.Source.Validate(diff.BaseSHA, diff.HeadSHA, current.Bundle)
	}
	if captureSource == nil {
		return Retention{}, errors.New("the finalized Change source capture is unavailable; resume on the source-owning runner before publication")
	}
	capture, err := captureSource(ctx, diff.BaseSHA, diff.HeadSHA)
	if err != nil {
		return Retention{}, err
	}
	if err := capture.Source.Validate(diff.BaseSHA, diff.HeadSHA, capture.Bundle); err != nil {
		return Retention{}, err
	}
	current = &capture
	return Retention{Capture: current}, nil
}
