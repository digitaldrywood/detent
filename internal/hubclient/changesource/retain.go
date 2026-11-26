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
		return Retention{Capture: current}, validateCapture(diff, *current)
	}
	if captureSource == nil {
		return Retention{}, errors.New("the finalized Change source capture is unavailable; resume on the source-owning runner before publication")
	}
	capture, err := captureSource(ctx, diff.BaseSHA, diff.HeadSHA)
	if err != nil {
		return Retention{}, err
	}
	if err := validateCapture(diff, capture); err != nil {
		return Retention{}, err
	}
	current = &capture
	return Retention{Capture: current}, nil
}

func validateCapture(diff tracker.AttemptDiffRequest, capture tracker.ChangeSourceCapture) error {
	source := capture.Source
	if source.Format != tracker.ChangeSourceGitPhase {
		return source.Validate(diff.BaseSHA, diff.HeadSHA, capture.Bundle)
	}
	if source.BaseSHA != diff.BaseSHA || source.HeadSHA != diff.HeadSHA || source.DiffSHA256 == "" || len(capture.Bundle) != 0 {
		return errors.New("Git phase description differs from the finalized source")
	}
	return nil
}
