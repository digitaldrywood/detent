package integrationpage

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaintenanceBytes = 2 * attachment.MaxArchiveBytes

func (s *Service) Read(ctx context.Context, scope hubapi.Scope) (projectstore.Integration, error) {
	var result projectstore.Integration
	err := s.snapshot(ctx, scope, func(ctx context.Context) error {
		return s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := scope.Authorize(ctx, r, false); err != nil {
				return err
			}
			reader, ok := r.(projectstore.IntegrationPageReader)
			if !ok {
				return errors.New("integration page storage unavailable")
			}
			var err error
			result, err = reader.IntegrationPage(ctx, scope.Project)
			return err
		})
	})
	if errors.Is(err, corestore.ErrNotFound) {
		return result, corestore.ErrNotFound
	}
	return result, s.deps.RecordError(err)
}

func (s *Service) MaintenanceFeature() hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: "integration-pages", Bytes: MaintenanceBytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return err
		}
		_, err := s.Maintain(ctx, tracker.OrganizationID(tenant.Organization))
		return err
	}}
}

func (s *Service) Maintain(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	if err := hubmaintenance.RequireAllowance(ctx, string(organization), MaintenanceBytes); err != nil {
		return false, err
	}
	if _, ok := s.deps.Details.(*hubmaintenance.Details); !ok {
		return false, errors.New("maintenance requires byte-limited payload storage")
	}
	var candidate projectstore.IntegrationCandidate
	err := s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
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
	err = s.deps.Repository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(ctx, candidate.ProjectID)
		return err
	})
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	var value *projectstore.Integration
	if err == nil {
		page := Record(record)
		page.WorkflowMarkdown, err = s.deps.Workflow(ctx, record)
		if err != nil {
			return false, err
		}
		value = &page
	}
	err = s.deps.Repository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		writer, ok := w.(projectstore.IntegrationPageWriter)
		if !ok {
			return errors.New("integration page storage unavailable")
		}
		return writer.PublishIntegrationPage(ctx, candidate, value, s.deps.Now())
	})
	if err == nil && s.deps.Logger != nil {
		s.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "project integration page", slog.String("organization_id", string(organization)), slog.Int64("generation", candidate.Generation), slog.Duration("projection_lag", s.deps.Now().Sub(candidate.SourceAt)))
	}
	return err == nil, err
}

func (s *Service) Get(c echo.Context) error {
	var value projectstore.Integration
	var cold bool
	scope := hubapi.Scope{Facts: hubapi.Facts{Organization: tracker.OrganizationID(c.Param("organization"))}}
	err := s.snapshot(c.Request().Context(), scope, func(ctx context.Context) error {
		if err := s.deps.Resolve(ctx, c); err != nil {
			return err
		}
		var err error
		scope := s.deps.Scope(c)
		value, err = s.Read(ctx, scope)
		if errors.Is(err, corestore.ErrNotFound) {
			cold = true
			return nil
		}
		return err
	})
	if err == nil && cold {
		var record projectstore.Record
		record, err = s.deps.ReadProject(c.Request().Context(), s.deps.Scope(c))
		if err == nil {
			value = Record(record)
			value.WorkflowMarkdown, err = s.deps.Workflow(c.Request().Context(), record)
		}
	}
	if err != nil {
		return s.deps.Error(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	scope = s.deps.Scope(c)
	if err := s.deps.Audit(c, scope); err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, value)
}
