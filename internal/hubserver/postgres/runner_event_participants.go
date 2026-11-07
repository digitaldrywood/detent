package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (f *runnerEventSession) WithRunEventParticipant(_ context.Context, apply func(tenantstore.Writer) error) error {
	return apply(f)
}

func (f *runnerEventSession) RunnerCompletionFacts() (eventstore.CollaborationVersion, bool, error) {
	if !f.hasEvidence {
		return eventstore.CollaborationVersion{}, false, nil
	}
	return eventstore.CollaborationVersion(f.completionVersion.value), true, f.completionVersion.err
}
