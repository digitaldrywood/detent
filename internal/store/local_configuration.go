package store

import (
	"context"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const LocalOrganization tracker.OrganizationID = "local"
const LocalRunner = "local"

type LocalConfigurationStore interface {
	InitializeLocalConfiguration(context.Context, globalconfig.Config, *config.ModelSelection) (bool, error)
	LocalConfiguration(context.Context, globalconfig.Config) (globalconfig.Config, error)
	MutateLocalConfiguration(context.Context, globalconfig.Config, func(*globalconfig.Config, string) bool) error
	LocalProjectRank(context.Context, *projectsettings.RankChange) (projectsettings.Rank, error)
	LocalModelSelection(context.Context, tracker.ProjectID, *projectsettings.ModelSelectionChange) (projectsettings.ModelSelection, error)
	LocalAllowedProjects(context.Context, *AllowedProjectsChange) (AllowedProjects, error)
}

type AllowedProjects struct {
	Revision   int64               `json:"revision"`
	ProjectIDs []tracker.ProjectID `json:"project_ids"`
}

type AllowedProjectsChange struct {
	ExpectedRevision int64               `json:"expected_revision"`
	ProjectIDs       []tracker.ProjectID `json:"project_ids"`
}
