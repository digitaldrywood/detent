package hubserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const integrationPageMaintenanceBytes = 2 * attachment.MaxArchiveBytes

func (s *Service) readIntegrationPage(ctx context.Context, scope nativeScope) (ProjectIntegration, error) {
	var result ProjectIntegration
	err := s.readWorkItemPageSnapshot(ctx, scope, func(ctx context.Context) error {
		return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
				return err
			}
			reader, ok := r.(projectstore.IntegrationPageReader)
			if !ok {
				return errors.New("integration page storage unavailable")
			}
			var err error
			result, err = reader.IntegrationPage(ctx, scope.project)
			return err
		})
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, &nativeError{Code: "not_ready", Message: "Project integration is being prepared", status: http.StatusServiceUnavailable, publicMessage: true}
	}
	return result, projectRecordError(err)
}

func (s *Service) IntegrationPageMaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "integration-pages", Bytes: integrationPageMaintenanceBytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		_, err := s.MaintainIntegrationPage(ctx, tracker.OrganizationID(tenant.Organization))
		return err
	}}
}

func (s *Service) MaintainIntegrationPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), integrationPageMaintenanceBytes); err != nil {
		return false, err
	}
	if _, ok := s.runDetailStore().(*hubmaintenance.Details); !ok {
		return false, errors.New("maintenance requires byte-limited payload storage")
	}
	var candidate projectstore.IntegrationCandidate
	err := s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.IntegrationPageWriter)
		if !ok {
			return errors.New("integration page storage unavailable")
		}
		var err error
		candidate, err = writer.ClaimIntegrationPage(ctx)
		return err
	})
	if err != nil {
		return false, err
	}
	if candidate.ProjectID == "" {
		return false, nil
	}
	var record projectstore.Record
	err = s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(ctx, candidate.ProjectID)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	var value *ProjectIntegration
	if err == nil {
		page := projectIntegrationRecord(record)
		page.WorkflowMarkdown, err = s.projectRecordWorkflow(ctx, record)
		if err != nil {
			return false, err
		}
		value = &page
	}
	err = s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.IntegrationPageWriter)
		if !ok {
			return errors.New("integration page storage unavailable")
		}
		return writer.PublishIntegrationPage(ctx, candidate, value, s.config.now())
	})
	if err == nil && s.config.Logger != nil {
		s.config.Logger.LogAttrs(ctx, slog.LevelDebug, "project integration page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.config.now().Sub(candidate.SourceAt)))
	}
	return err == nil, err
}

func (s *Service) getProjectIntegrationPage(c echo.Context) error {
	var value ProjectIntegration
	scope := nativeScope{organization: tracker.OrganizationID(c.Param("organization"))}
	err := s.readWorkItemPageSnapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		if err := s.resolveNativeRequestScope(ctx, c); err != nil {
			return err
		}
		var err error
		value, err = s.readIntegrationPage(ctx, nativeRequestScope(c))
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	scope = nativeRequestScope(c)
	if scope.credential.Hosted != nil {
		if err := s.hostedAudit(c.Request().Context(), scope.credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, value)
}
