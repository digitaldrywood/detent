package hubscope

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AccessAuthorizer func(ctx context.Context, reader accessstore.Reader, scope NativeScope, write bool) error

type WorkItemAuthorizer func(ctx context.Context, reader accessstore.Reader, scope NativeScope) error

type Authorization struct {
	Access   AccessAuthorizer
	WorkItem WorkItemAuthorizer
}

func (a Authorization) AuthorizeInput(ctx context.Context, reader accessstore.Reader, input hubauth.Input, write bool) error {
	return a.Access(ctx, reader, ScopeFromAuthInput(input), write)
}

func (a Authorization) AuthorizeWorkItemInput(ctx context.Context, reader accessstore.Reader, input hubauth.Input) error {
	return a.WorkItem(ctx, reader, ScopeFromAuthInput(input))
}

func (a Authorization) Scope(scope NativeScope) hubapi.Scope {
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
		return a.Access(ctx, reader, rebound, write)
	})
}

func ScopeFromAuthInput(input hubauth.Input) NativeScope {
	return NativeScope{Organization: input.Organization, Project: input.Project, Credential: NativeCredential(input.Credential), RequireHostedAdmin: input.Admin, Action: input.Action}
}
