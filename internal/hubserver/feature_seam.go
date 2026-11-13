package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) featureScope(scope nativeScope) hubapi.Scope {
	facts := hubapi.Facts{
		Organization: scope.organization,
		Project:      scope.project,
		Actor:        scope.actor(),
		CredentialID: scope.credential.ID,
		HostedRole:   scope.credential.HostedRole,
		Admin:        scope.requireHostedAdmin,
		Runner:       scope.credential.Runner,
	}
	return hubapi.NewProjectScope(facts, func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
		rebound := scope
		rebound.project = project
		return s.authorizeAccessScope(ctx, reader, rebound, write)
	})
}
