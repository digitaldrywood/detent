package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) requestMetricsVisible(ctx context.Context) bool {
	if s.requestMetrics == nil {
		return false
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeAdmin, OrganizationID: string(s.requestMetrics.organization), OrganizationWide: true}); err != nil {
		return false
	}
	credential, err := currentHubOperator(ctx)
	return err == nil && (credential.HostedKeyScope == "" || credential.HostedProjectAccess == hostedProjectsAll)
}

func (s *Service) adminRequestMetrics(ctx context.Context) (requestMetricsReport, bool, error) {
	if !s.requestMetricsVisible(ctx) {
		return requestMetricsReport{}, false, nil
	}
	report, err := s.requestMetrics.report(ctx, s.config.now())
	return report, true, err
}
