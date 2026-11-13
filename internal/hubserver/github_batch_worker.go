package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const githubBatchMaintenanceLimit = 100

func (s *Service) maintainGitHubBatches(ctx context.Context) error {
	if s.config.ImportBackend == nil {
		return nil
	}
	if s.config.TenantRepository == nil || s.database == nil {
		return hubapi.ErrPostgresFeatureUnavailable
	}
	var project tracker.ProjectID
	organization := s.database.hostedOrganization
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return hubapi.ErrPostgresFeatureUnavailable
		}
		project, err = gitHubIntakeReader.NextGitHubIntake(ctx, s.config.now())
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	scope, err := s.gitHubIntegrationScope(ctx, organization, project)
	if err == nil {
		err = s.advanceGitHubBatchRecord(ctx, scope)
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		retryErr := s.config.TenantRepository.WriteTenant(cleanup, organization, corestore.Background, func(w tenantstore.Writer) error {
			gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
			if !ok {
				return hubapi.ErrPostgresFeatureUnavailable
			}
			return gitHubIntakeWriter.DeferGitHubIntake(cleanup, project, s.config.now(), s.config.deps.Jitter())
		})
		if errors.Is(err, corestore.ErrNotFound) {
			return retryErr
		}
		return errors.Join(err, retryErr)
	}
	return nil
}

func (s *Service) completeRunnerGitHubBatch(ctx context.Context, scope hubscope.NativeScope) error {
	return s.advanceGitHubBatchRecord(ctx, scope)
}

func (s *Service) advanceGitHubBatch(ctx context.Context, scope hubscope.NativeScope) error {
	return s.advanceGitHubBatchRecord(ctx, scope)
}

func (s *Service) gitHubIntegrationScope(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (hubscope.NativeScope, error) {
	scope := hubscope.NativeScope{IntegrationOwned: true, Organization: organization, Project: project, Credential: hubapi.APICredential{ID: "github", Scope: hubapi.APIScopeAdmin}, SourceActor: nativeIntegrationActor("github")}
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		p, err := r.ProjectRecord(ctx, project)
		if err != nil {
			return err
		}
		if p.Core.DeletedAt != nil || p.Core.Profile != "native" {
			return corestore.ErrNotFound
		}
		scope.IntegrationRepository = p.Settings.CheckoutRepository
		if scope.IntegrationRepository == "" {
			scope.IntegrationRepository = p.Settings.Repository
		}
		if scope.IntegrationRepository == "" {
			return corestore.ErrNotFound
		}
		return nil
	})
	return scope, err
}
