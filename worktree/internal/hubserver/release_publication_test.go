package hubserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type releasePublisherFixture struct {
	*releaseVerifierFixture
	creates      int
	beforeCreate func()
	lostResponse bool
	publication  tracker.ReleasePickPublication
}

func (p *releasePublisherFixture) PublishReleasePick(ctx context.Context, item tracker.NativeWorkItemID, input tracker.ReleasePickInput, authorize func(context.Context) error) (tracker.ReleasePickPublication, error) {
	if p.beforeCreate != nil {
		p.beforeCreate()
	}
	if err := authorize(ctx); err != nil {
		return tracker.ReleasePickPublication{}, err
	}
	if input.PRNumber > 0 {
		if err := p.VerifyReleasePick(ctx, input); err != nil {
			return tracker.ReleasePickPublication{}, err
		}
		return tracker.ReleasePickPublication{ReleasePickInput: input, WorkItemID: item, URL: fmt.Sprintf("%s/pull/%d", input.Repository, input.PRNumber)}, nil
	}
	expected := p.input
	expected.PRNumber = 0
	if input != expected {
		return tracker.ReleasePickPublication{}, ErrReleasePickMismatch
	}
	if p.creates == 0 {
		p.creates++
		p.publication = tracker.ReleasePickPublication{ReleasePickInput: p.input, WorkItemID: item, URL: "https://github.com/example-org/example-project/pull/7"}
	}
	if p.lostResponse {
		p.lostResponse = false
		return tracker.ReleasePickPublication{}, errors.New("lost create response")
	}
	return p.publication, nil
}
