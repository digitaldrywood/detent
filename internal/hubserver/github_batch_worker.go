package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const githubBatchMaintenanceLimit = 100

func (s *Service) maintainGitHubBatches(ctx context.Context) error {
	if s.config.ImportBackend == nil || s.config.TenantRepository == nil {
		return nil
	}
	var project tracker.ProjectID
	organization := s.database.hostedOrganization
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		gitHubIntakeReader, ok := r.(projectstore.GitHubIntakeReader)
		if !ok {
			return ErrPostgresFeatureUnavailable
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
	if err != nil {
		return err
	}
	err = s.advanceGitHubBatchRecord(ctx, scope)
	if err != nil {
		retryErr := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			gitHubIntakeWriter, ok := w.(projectstore.GitHubIntakeWriter)
			if !ok {
				return ErrPostgresFeatureUnavailable
			}
			return gitHubIntakeWriter.DeferGitHubIntake(ctx, project, s.config.now())
		})
		return errors.Join(err, retryErr)
	}
	return nil
}

func (s *Service) completeRunnerGitHubBatch(ctx context.Context, scope nativeScope) error {
	return s.advanceGitHubBatchRecord(ctx, scope)
}

func (s *Service) advanceGitHubBatch(ctx context.Context, scope nativeScope) error {
	return s.advanceGitHubBatchRecord(ctx, scope)
}

func (s *Service) gitHubIntegrationScope(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID) (nativeScope, error) {
	scope := nativeScope{integrationOwned: true, organization: organization, project: project, credential: apiCredential{ID: "github", Scope: apiScopeAdmin}, sourceActor: nativeIntegrationActor("github")}
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		p, err := r.ProjectRecord(ctx, project)
		if err != nil {
			return err
		}
		if p.Core.DeletedAt != nil || p.Core.Profile != "native" {
			return corestore.ErrNotFound
		}
		scope.integrationRepository = p.Settings.CheckoutRepository
		if scope.integrationRepository == "" {
			scope.integrationRepository = p.Settings.Repository
		}
		if scope.integrationRepository == "" {
			return corestore.ErrNotFound
		}
		return nil
	})
	return scope, err
}
