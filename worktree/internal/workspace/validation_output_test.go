package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

type validationProcessFixture struct {
	started  func(procgroup.Identity) error
	finished func(gate.CommandResult, error) error
}

func (p validationProcessFixture) Started(_ context.Context, identity procgroup.Identity) error {
	return p.started(identity)
}
func (p validationProcessFixture) Finished(_ context.Context, result gate.CommandResult, err error) error {
	return p.finished(result, err)
}
