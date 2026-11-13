package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/githubreconcile"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) githubReconcileService() *githubreconcile.Service {
	var organization tracker.OrganizationID
	if s.database != nil {
		organization = s.database.hostedOrganization
	}
	return githubreconcile.New(githubreconcile.Deps{Repository: s.config.TenantRepository, Organization: organization, Backend: s.config.ReconcileBackend, Importer: s.config.ImportBackend, Ready: s.database != nil, Unavailable: ErrPostgresFeatureUnavailable, Now: s.config.now, Jitter: s.config.jitter, Interval: s.config.ReconcileInterval,
		Integration: func(ctx context.Context, org tracker.OrganizationID, project tracker.ProjectID) (githubreconcile.Integration, error) {
			scope, err := s.gitHubIntegrationScope(ctx, org, project)
			return githubreconcile.Integration{Repository: scope.integrationRepository, Apply: func(ctx context.Context, item tracker.NativeWorkItemID, operation string, request tracker.GitHubIntake) error {
				_, err := s.applyLinkedIssueRecord(ctx, scope, item, operation, request)
				return err
			}}, err
		}})
}
func (s *Service) maintainGitHubReconciliationRecords(ctx context.Context) error {
	return s.githubReconcileService().Maintain(ctx)
}
func (s *Service) GitHubReconciliationMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "github-reconciliation", Bytes: 4 << 20, Step: func(ctx context.Context, selected hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
			return err
		}
		return s.maintainGitHubReconciliationRecords(ctx)
	}}
}

func (s *Service) maintainGitHubPullRequestRefresh(ctx context.Context) error {
	return s.githubReconcileService().MaintainPullRequests(ctx)
}
