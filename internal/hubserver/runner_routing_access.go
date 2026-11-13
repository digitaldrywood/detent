package hubserver

import (
	"context"
	"encoding/json"
	"errors"
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

func (s *Service) authorizeRunnerRoutingAccess(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, projects []tracker.ProjectID, routingScope string) error {
	if err := s.authorizeAccessScope(ctx, reader, scope, true); err != nil {
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
		if err := s.authorizeAccessScope(ctx, reader, selected, true); err != nil {
			return err
		}
		if scope.Credential.Hosted != nil {
			member, err := reader.AccessMember(ctx, scope.Credential.Hosted.Subject)
			if err != nil {
				return err
			}
			if hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole) == "member" {
				grant, err := reader.AccessProjectGrant(ctx, member.UserID, project)
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
			allowed, err := s.appAllRunnerGrantsWith(ctx, reader, credential, nil)
			if errors.Is(err, corestore.ErrInvalidPage) {
				return hubapi.NativeNotFound()
			}
			if err != nil {
				return err
			}
			if !allowed {
				return hubapi.NativeNotFound()
			}
		}
	}
	return nil
}

func (s *Service) readRunnerRoutingReceipt(ctx context.Context, scope hubscope.NativeScope, resource string, receipt eventstore.Receipt, content *eventstore.Content) (json.RawMessage, error) {
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
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.authorizeRunnerRoutingAccess(ctx, reader, scope, projects, represented.Scope)
	}); err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	return raw, nil
}
