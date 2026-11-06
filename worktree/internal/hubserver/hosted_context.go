package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var errHostedContextUnavailable = errors.New("hosted application context is unavailable")

type hostedContextExecutor struct{ service *Service }

func (e hostedContextExecutor) credential(ctx context.Context, name, project string) (context.Context, apiCredential, error) {
	requirement := operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: project}
	if name == operatortool.AppUpdates {
		requirement.ResourceKind = "runners"
	}
	ctx, err := operatortool.AuthorizeCurrent(ctx, requirement)
	if err != nil {
		return ctx, apiCredential{}, err
	}
	s := e.service
	if s.config.Hosted == nil {
		return ctx, apiCredential{}, errHostedContextUnavailable
	}
	if s.config.TenantRepository == nil || name != operatortool.AppUpdates && name != operatortool.AppBootstrapPayload {
		return ctx, apiCredential{}, errHostedContextUnavailable
	}
	credential, err := currentHubOperator(ctx)
	if err != nil || credential.Hosted == nil || credential.HostedRole == "account" || operatorIdentity(credential, s.config.Hosted.OrganizationID) != operatortool.ConnectionIdentity(ctx) {
		return ctx, apiCredential{}, operatortool.ErrAccessDenied
	}
	if name == operatortool.AppUpdates && !s.appAllRunnerGrants(ctx, credential) {
		return ctx, apiCredential{}, operatortool.ErrAccessDenied
	}
	return ctx, credential, nil
}

func (e hostedContextExecutor) listTools(ctx context.Context) []operatortool.Definition {
	s := e.service
	if s.config.Hosted == nil {
		return nil
	}
	if s.config.TenantRepository == nil {
		return nil
	}
	catalog, err := operatorCatalog(ctx)
	if err != nil || catalog.credential.Hosted == nil || catalog.credential.HostedRole == "account" || operatorIdentity(catalog.credential, s.config.Hosted.OrganizationID) != operatortool.ConnectionIdentity(ctx) {
		return nil
	}
	definitions := []operatortool.Definition{}
	for _, definition := range operatortool.HostedContextCatalog() {
		requirement := operatortool.Requirement{Scope: apikey.ScopeRead}
		switch definition.Name {
		case operatortool.AppUpdates:
			requirement.ResourceKind = "runners"
			if !s.appAllRunnerGrants(ctx, catalog.credential) {
				continue
			}
		case operatortool.AppBootstrapPayload:
		default:
			continue
		}
		if _, err := s.authorizeCatalog(ctx, requirement); err == nil {
			definitions = append(definitions, definition)
		}
	}
	return definitions
}

func (e hostedContextExecutor) Execute(ctx context.Context, call operatortool.Call) (result operatortool.Result, resultErr error) {
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	defer func() {
		if resultErr == nil {
			return
		}
		switch {
		case errors.Is(resultErr, operatortool.ErrAccessDenied):
			resultErr = operatortool.ErrAccessDenied
		case errors.Is(resultErr, operatortool.ErrInvalidArguments):
			resultErr = operatortool.ErrInvalidArguments
		default:
			resultErr = errHostedContextUnavailable
		}
	}()
	switch call.Name {
	case operatortool.AppBootstrapPayload, operatortool.AppUpdates:
		if err := operatortool.DecodeArguments(call.Arguments, &struct{}{}); err != nil {
			return result, err
		}
	default:
		return result, operatortool.ErrUnknownTool
	}
	ctx, credential, err := e.credential(ctx, call.Name, "")
	if err != nil {
		return result, err
	}
	s := e.service
	var payload any
	switch call.Name {
	case operatortool.AppBootstrapPayload:
		session := auth.Session{}
		if credential.SessionHash != "" {
			session, err = s.storedWebSession(ctx, credential.SessionHash, s.config.now())
			if err != nil {
				return result, operatortool.ErrAccessDenied
			}
		} else if credential.HostedKeyScope != "" {
			member, err := s.activeAccessMember(readCtx, credential.Hosted.Subject)
			if err != nil {
				return result, operatortool.ErrAccessDenied
			}
			session.Email = member.Email
		}
		var bootstrap appBootstrap
		bootstrap, err = s.readPostgresAppBootstrap(readCtx, credential, session)
		if err == nil {
			var seen time.Time
			seen, err = s.releaseSeenTime(readCtx, credential)
			if err == nil {
				bootstrap.HasUnread = s.releasesUnread(seen)
			}
		}
		if err == nil && s.database.plans() != nil {
			var entitlement HostedEntitlement
			entitlement, err = s.database.hostedEntitlement(readCtx, nil, s.config.now())
			if err == nil {
				bootstrap.Plan = &appBootstrapPlan{ID: entitlement.EffectiveBase.ID, Name: entitlement.Name, Source: entitlement.Source, WindowEndsAt: entitlement.WindowEndsAt.UTC().Format(time.RFC3339)}
			}
		}
		if err != nil {
			return result, err
		}
		if len(bootstrap.Projects) > operatortool.MaxItemLimit || len(bootstrap.Organizations) > operatortool.MaxItemLimit || len(bootstrap.Preferences.Models) > operatortool.MaxItemLimit {
			return result, errHostedContextUnavailable
		}
		payload = struct {
			ObservedAt string `json:"observed_at"`
			Freshness  string `json:"freshness"`
			appBootstrap
		}{s.config.now().UTC().Format(time.RFC3339Nano), "current_observation", bootstrap}
	case operatortool.AppUpdates:
		updates, err := s.readAppUpdates(ctx, credential)
		if err != nil {
			return result, err
		}
		if len(updates.Runners) > operatortool.MaxItemLimit {
			return result, errHostedContextUnavailable
		}
		payload = struct {
			ObservedAt string `json:"observed_at"`
			Freshness  string `json:"freshness"`
			appUpdates
		}{s.config.now().UTC().Format(time.RFC3339Nano), "current_observation", updates}

	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) > operatortool.MaxResultBytes {
		return result, errHostedContextUnavailable
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "mcp "+call.Name, "", http.StatusOK); err != nil {
		return result, err
	}
	return operatortool.Result{Content: raw}, nil
}

func (s *Service) appAllRunnerGrants(ctx context.Context, credential apiCredential, projects ...tracker.ProjectID) bool {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if credential.Hosted == nil {
		return false
	}
	return s.appAllRunnerGrantsPostgres(ctx, credential, projects)
}

func (s *Service) appAllRunnerGrantsPostgres(ctx context.Context, credential apiCredential, projects []tracker.ProjectID) bool {
	if s.config.TenantRepository == nil {
		return false
	}
	var allowed bool
	err := s.config.TenantRepository.ReadTenant(ctx, tracker.OrganizationID(s.config.Hosted.OrganizationID), corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		allowed, err = s.appAllRunnerGrantsWith(ctx, reader, credential, projects)
		return err
	})
	return err == nil && allowed
}

func (s *Service) appAllRunnerGrantsWith(ctx context.Context, reader tenantstore.Reader, credential apiCredential, projects []tracker.ProjectID) (bool, error) {
	if credential.HostedRole != "owner" && credential.HostedRole != "admin" && credential.HostedRole != "member" || projects != nil && len(projects) == 0 {
		return false, nil
	}
	if credential.HostedKeyScope == "" && (credential.HostedRole == "owner" || credential.HostedRole == "admin") {
		return true, nil
	}
	var total, accessible int
	err := func() error {
		accessReader, ok := reader.(accessstore.ProjectAccessPageReader)
		if !ok {
			return ErrProjectParticipants
		}
		if projects != nil {
			if len(projects) > corestore.MaxPageSize {
				return corestore.ErrInvalidPage
			}
			for _, project := range projects {
				rows, err := accessReader.ProjectAccessPage(ctx, projectAccessFilter(credential, project, "", 1))
				if err != nil {
					return err
				}
				if len(rows) == 0 || rows[0].ProjectID != project || credential.HostedRole == "member" && !rows[0].ManageRunner {
					return nil
				}
				accessible++
			}
			total = len(projects)
			return nil
		}
		page, err := accessReader.ProjectAccessPage(ctx, projectAccessFilter(credential, "", "", corestore.MaxPageSize))
		if err != nil {
			return err
		}
		if len(page) > corestore.MaxPageSize {
			return corestore.ErrInvalidPage
		}
		if credential.HostedRole == "member" {
			for _, project := range page {
				if !project.ManageRunner {
					return operatortool.ErrAccessDenied
				}
			}
		}
		accessible = len(page)
		projectsPage, err := reader.Projects(ctx, "", corestore.MaxPageSize)
		if err != nil {
			return err
		}
		if len(projectsPage) >= corestore.MaxPageSize {
			return corestore.ErrInvalidPage
		}
		for _, project := range projectsPage {
			if project.DeletedAt == nil {
				total++
			}
		}
		return nil
	}()
	return err == nil && total > 0 && total == accessible, err
}
