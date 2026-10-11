package cloudentry

import (
	"context"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"
)

func (s *Service) checkTenant(ctx context.Context, organization Organization) error {
	status, err := s.serviceRequest(ctx, organization, "/internal/v1/health", map[string]string{}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("tenant health returned HTTP %d", status)
	}
	return nil
}

func (s *Service) servingTenants(ctx context.Context, organizations []Organization) (int, int) {
	var ids []string
	for _, organization := range organizations {
		if organization.State == "ready" {
			ids = append(ids, organization.ID)
		}
	}
	healthy := make([]bool, len(ids))
	s.eachReadyTenant(ctx, ids, func(call context.Context, index int, organization Organization) {
		healthy[index] = s.checkTenant(call, organization) == nil
	})
	running := 0
	for _, ok := range healthy {
		if ok {
			running++
		}
	}
	return len(ids), running
}

func (s *Service) health(c echo.Context) error {
	ctx := c.Request().Context()
	organizations, err := s.registry.List(ctx)
	status, code := "unavailable", http.StatusServiceUnavailable
	if err == nil && s.auth.store.db.PingContext(ctx) == nil {
		if expected, running := s.servingTenants(ctx, organizations); expected == running {
			status, code = "ok", http.StatusOK
		}
	}
	return c.JSON(code, map[string]string{"status": status, "version": s.config.Build.Version, "commit": s.config.Build.Commit})
}
