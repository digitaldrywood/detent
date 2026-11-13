package githubcontract

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrReleasePickMismatch = errors.New("release pick provenance mismatch")

type ReleasePickVerifier interface {
	VerifyReleasePick(context.Context, tracker.ReleasePickInput) error
	VerifyReleasePickRefresh(context.Context, tracker.ReleasePickInput, tracker.ReleasePickInput) error
}

type ReleaseDeliveryVerifier interface {
	ReleaseDeliveryStatus(context.Context, tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error)
}

type ReleasePickPublisher interface {
	PublishReleasePick(context.Context, tracker.NativeWorkItemID, tracker.ReleasePickInput, func(context.Context) error) (tracker.ReleasePickPublication, error)
}
