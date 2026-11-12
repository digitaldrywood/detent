package hubserver

import (
	"context"
	"errors"

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

func fromFeatureError(err error) error {
	var failure *hubapi.Error
	if !errors.As(err, &failure) || failure == nil {
		return err
	}
	return &nativeError{Code: failure.Code, Message: failure.Message, CurrentRevision: failure.CurrentRevision, Details: failure.Details, status: failure.Status, publicMessage: failure.Public, logAttributes: failure.LogAttributes()}
}
