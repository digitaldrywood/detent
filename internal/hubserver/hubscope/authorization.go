package hubscope

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
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

func accessInput(scope NativeScope) hubauth.Input {
	return hubauth.Input{Organization: scope.Organization, Project: scope.Project, Credential: AuthCredential(scope.Credential), Admin: scope.RequireHostedAdmin, Action: scope.Action, IntegrationRepository: scope.IntegrationRepository, IntegrationOwned: scope.IntegrationOwned}
}

func AuthInput(scope NativeScope, authorizeBatch func(context.Context, tenantstore.Reader, NativeScope, bool) error, authorizeRunner func(context.Context, tenantstore.Reader, NativeScope, string, string, time.Time) error, now func() time.Time) hubauth.Input {
	input := accessInput(scope)
	if scope.GitHubIntakeRevision != nil {
		input.IntegrationFence = func(ctx context.Context, r accessstore.Reader) error {
			if fence := scope.GitHubIntakeRevision; fence != nil {
				if fence.BatchID != "" {
					owner, ok := r.(projectstore.IntakeBatchReader)
					if !ok {
						return accessstore.ErrDenied
					}
					batch, err := owner.ProjectIntakeBatch(ctx, scope.Project)
					if err != nil {
						return err
					}
					if batch.ID != fence.BatchID || batch.Revision != fence.Revision || batch.Status != "discovering" && batch.Status != "importing" {
						return corestore.ErrRevisionConflict
					}
					if fence.Authority == nil {
						return operatortool.ErrAccessDenied
					}
					reader, ok := r.(tenantstore.Reader)
					if !ok {
						return hubapi.ErrPostgresFeatureUnavailable
					}
					if err := authorizeBatch(ctx, reader, *fence.Authority, fence.RequireAdminKey); err != nil {
						return err
					}
					if fence.Reporter != nil {
						if err := authorizeRunner(ctx, reader, *fence.Reporter, fence.Reporter.Credential.Runner.RunnerID, scope.IntegrationRepository, now()); err != nil {
							return err
						}
					}

				}
				if fence.ImportID != "" {
					owner, ok := r.(projectstore.GitHubImportReader)
					if !ok {
						return accessstore.ErrDenied
					}
					job, err := owner.GitHubImport(ctx, scope.Project, fence.ImportID)
					if err != nil {
						return err
					}
					if job.Revision != fence.Revision {
						return corestore.ErrRevisionConflict
					}
				}
			}
			return nil
		}
	}
	return input
}

func AuthorizeWorkItem(ctx context.Context, reader accessstore.Reader, scope NativeScope, authorize AccessAuthorizer, now func() time.Time) error {
	if err := authorize(ctx, reader, scope, true); err != nil {
		return err
	}
	if scope.Credential.Hosted == nil {
		return nil
	}
	member, err := reader.AuthorizeAccess(ctx, hubauth.Authority(accessInput(scope), true), now())
	if err != nil {
		return err
	}
	if member.UserID != "" && hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) != scope.Credential.HostedRole {
		return auth.ErrHostedIdentity
	}
	return nil
}

func AuthorizeConfiguration(ctx context.Context, r accessstore.Reader, scope NativeScope, operation string, authorize WorkItemAuthorizer) error {
	if scope.Credential.Runner.RunnerID != "" {
		return hubapi.NativeNotFound()
	}
	if scope.Credential.Hosted == nil && scope.Credential.EntryKey == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return hubapi.NativeNotFound()
	}
	scope.RequireHostedAdmin = true
	if err := authorize(ctx, r, scope); err != nil {
		return err
	}
	if operation == "resume_local_project" && scope.Credential.Hosted != nil {
		grant, err := r.AccessProjectGrant(ctx, scope.Credential.Hosted.Subject, scope.Project, scope.Credential.HostedRole)
		if err != nil {
			return err
		}
		if !grant.ManageRunner {
			return hubapi.NativeNotFound()
		}
	}
	return nil
}
