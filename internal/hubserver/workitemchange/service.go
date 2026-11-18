package workitemchange

import (
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository        tenantstore.Repository
	PullRequestNumber func(tracker.ChangeExternalReference) int
}

type Snapshot struct {
	Project            projectstore.Record
	Attempt            *leasestore.Attempt
	Diff               *leasestore.AttemptDiff
	Publication        *eventstore.NativePRPublication
	Change             *eventstore.Change
	ChangeVersion      *eventstore.Version
	PullRequestVersion *eventstore.PullRequestVersion
}

type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
