package hubserver

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) projectOnboardingBatch(ctx context.Context, organization tracker.OrganizationID, projects []appBootstrapProject) (map[tracker.ProjectID]onboarding.Project, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if len(projects) == 0 {
		return map[tracker.ProjectID]onboarding.Project{}, nil
	}
	if s.config.TenantRepository == nil {
		return nil, &nativeError{Code: "storage_owner_unavailable", Message: "Project setup storage is unavailable", status: http.StatusServiceUnavailable, publicMessage: true}
	}
	return s.projectOnboardingBatchTenant(ctx, organization, projects)
}
