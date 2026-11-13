package hubserver

import (
	"context"
	"errors"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (s *Service) reportsReadableProjects(ctx context.Context, credential hubapi.APICredential, project string) ([]string, bool, error) {
	granted, err := s.usageReadableProjects(ctx, credential)
	if err != nil {
		return nil, false, err
	}
	if project != reportsAllProjects {
		return []string{project}, slices.Contains(granted, project), nil
	}
	readable := slices.DeleteFunc(granted, func(id string) bool {
		_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: id})
		return err != nil
	})
	return readable, len(readable) > 0, nil
}

func (s *Service) hostedReports(c echo.Context) error {
	return s.hostedReportsPostgres(c)
}

func (s *Service) hostedReportOptions(c echo.Context) error {
	return s.hostedReportOptionsPostgres(c)
}

func (s *Service) readProjectReport(ctx context.Context, credential hubapi.APICredential, project string, w operatortool.AnalyticsWindow, filter reportsFilter) (hostedReportsReport, bool, error) {
	if s.config.TenantRepository == nil {
		return hostedReportsReport{}, false, errors.New("PostgreSQL report reader is unavailable")
	}
	projects, err := s.analyticsReadableProjects(ctx, credential, operatortool.AnalyticsRequest{ProjectID: project})
	if err != nil || len(projects) != 1 {
		return hostedReportsReport{}, false, err
	}
	report, err := s.readPostgresHostedReport(ctx, credential, projects, w, filter, true)
	return report, err == nil, err
}
