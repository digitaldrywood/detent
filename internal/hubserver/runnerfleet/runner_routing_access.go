package runnerfleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (d RoutingDeps) authorizeRunnerRoutingAccess(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, projects []tracker.ProjectID, routingScope string) error {
	if err := d.AuthorizeAccess(ctx, reader, scope, true); err != nil {
		return err
	}
	for _, project := range projects {
		row, err := reader.Project(ctx, project)
		if err != nil {
			return err
		}
		if row.DeletedAt != nil {
			return corestore.ErrNotFound
		}
		selected := scope
		selected.Project = project
		if err := d.AuthorizeAccess(ctx, reader, selected, true); err != nil {
			return err
		}
		if scope.Credential.Hosted != nil {
			member, err := reader.AccessMember(ctx, scope.Credential.Hosted.Subject)
			if err != nil {
				return err
			}
			if role := hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole); role == "member" {
				grant, err := reader.AccessProjectGrant(ctx, member.UserID, project, role)
				if err != nil || !grant.ManageRunner {
					return accessstore.ErrDenied
				}
			}
		}
	}
	if scope.Credential.Hosted != nil && routingScope == "organization" {
		credential := scope.Credential
		member, err := reader.AccessMember(ctx, credential.Hosted.Subject)
		if err != nil {
			return err
		}
		key, err := reader.AccessCredential(ctx, credential.ID)
		if err != nil {
			return err
		}
		credential.HostedRole = hubapi.LesserHostedRole(member.Role, credential.HostedRole)
		credential.HostedKeyScope = apikey.Scope(key.KeyScope)
		credential.HostedProjectAccess = hubapi.HostedProjectAccess(key.ProjectAccess)
		organizationAdmin := (credential.HostedRole == "owner" || credential.HostedRole == "admin") && (credential.HostedKeyScope == "" || credential.HostedProjectAccess == hubapi.HostedProjectsAll)
		if !organizationAdmin {
			allowed, err := d.AllRunnerGrantsWith(ctx, reader, credential, nil)
			if errors.Is(err, corestore.ErrInvalidPage) {
				return hubapi.NativeNotFound()
			}
			if err != nil {
				return err
			}
			if !allowed {
				return &hubapi.NativeError{Code: "forbidden", Message: "Organization-wide runner scope needs manage_runner on every project; ask an owner or admin", Status: http.StatusForbidden, PublicMessage: true}
			}
		}
	}
	return nil
}

func (d RoutingDeps) readRunnerRoutingReceipt(ctx context.Context, scope hubscope.NativeScope, resource string, receipt eventstore.Receipt, content *eventstore.Content) (json.RawMessage, error) {
	raw, err := content.Receipt(ctx, receipt)
	if err != nil {
		return nil, err
	}
	var represented runnerauth.Runner
	if err := json.Unmarshal(raw, &represented); err != nil {
		return nil, artifact.ErrIntegrity
	}
	if represented.OrganizationID != scope.Organization || represented.RunnerID != resource || represented.Scope != "projects" && represented.Scope != "organization" {
		return nil, artifact.ErrIntegrity
	}
	projects := []tracker.ProjectID{}
	if represented.Scope != "organization" {
		projects = append(projects, represented.ProjectIDs...)
		for project := range represented.ProjectRanks {
			projects = append(projects, project)
		}
		for project := range represented.ProjectCapacityLimits {
			projects = append(projects, project)
		}
		for project := range represented.ProjectUsed {
			projects = append(projects, project)
		}
		for _, lease := range represented.Leases {
			if lease.ProjectID != "" {
				projects = append(projects, lease.ProjectID)
			}
		}
		for _, problem := range represented.Problems {
			if problem.ProjectID != "" {
				projects = append(projects, tracker.ProjectID(problem.ProjectID))
			}
		}
		slices.Sort(projects)
		projects = slices.Compact(projects)
	}
	if err := d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return d.authorizeRunnerRoutingAccess(ctx, reader, scope, projects, represented.Scope)
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return raw, nil
}
