package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) featureScope(scope hubscope.NativeScope) hubapi.Scope {
	facts := hubapi.Facts{
		Organization: scope.Organization,
		Project:      scope.Project,
		Actor:        scope.Actor(),
		CredentialID: scope.Credential.ID,
		HostedRole:   scope.Credential.HostedRole,
		Admin:        scope.RequireHostedAdmin,
		Runner:       scope.Credential.Runner,
	}
	return hubapi.NewProjectScope(facts, func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
		rebound := scope
		rebound.Project = project
		return s.authorizeAccessScope(ctx, reader, rebound, write)
	})
}
