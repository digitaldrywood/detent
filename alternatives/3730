package projectstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OrganizationReader interface {
	OrganizationSchedulingRevision(context.Context) (int64, error)
	OrganizationRankProjects(context.Context, int) ([]corestore.Project, error)
}

type OrganizationWriter interface {
	OrganizationReader
	SetOrganizationProjectRanks(context.Context, int64, []tracker.ProjectID) error
}

type OrganizationSchedulingRank struct {
	ProjectID tracker.ProjectID
	Rank      int64
}

type OrganizationSchedulingAdoption struct {
	OrganizationID   tracker.OrganizationID
	ManifestSHA256   [32]byte
	SourceSHA256     [32]byte
	ExpectedRevision int64
	SourceRevision   int64
	Projects         []OrganizationSchedulingRank
}

type OrganizationSchedulingImporter interface {
	AdoptOrganizationSchedulingRevision(context.Context, OrganizationSchedulingAdoption) error
}
