package cloudentry

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

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
	rows := s.tenantHealth(ctx, organizations)
	running := 0
	for _, row := range rows {
		if row.Running {
			running++
		}
	}
	return len(rows), running
}

type platformTenantHealth struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Running       bool   `json:"running"`
	UptimeSeconds *int64 `json:"uptime_seconds"`
	Restarts24h   *int   `json:"restarts_24h"`
	DatabaseBytes *int64 `json:"database_bytes"`
}

func (s *Service) tenantHealth(ctx context.Context, organizations []Organization) []platformTenantHealth {
	var ids []string
	rows := []platformTenantHealth{}
	for _, organization := range organizations {
		if organization.State == "ready" {
			ids = append(ids, organization.ID)
			rows = append(rows, platformTenantHealth{ID: organization.ID, Name: organization.Name})
		}
	}
	s.eachReadyTenant(ctx, ids, func(call context.Context, index int, organization Organization) {
		row := &rows[index]
		row.Running = s.checkTenant(call, organization) == nil
		if allocation := s.config.Allocation; allocation != nil && organization.Managed {
			if reader, ok := allocation.Launcher.(interface {
				Health(string, time.Time) (*int64, *int)
			}); ok {
				row.UptimeSeconds, row.Restarts24h = reader.Health(organization.ID, s.config.now())
			}
			databasePath := filepath.Join(allocation.TenantRoot, organization.ID, "hub.db")
			if info, err := os.Stat(databasePath); err == nil && info.Mode().IsRegular() {
				size := info.Size()
				row.DatabaseBytes = &size
				for _, suffix := range []string{"-wal", "-shm"} {
					sidecar, err := os.Stat(databasePath + suffix)
					if os.IsNotExist(err) {
						continue
					}
					if err != nil || !sidecar.Mode().IsRegular() {
						row.DatabaseBytes = nil
						break
					}
					size += sidecar.Size()
				}
			}
		}
		if !row.Running {
			row.UptimeSeconds = nil
		}
	})
	return rows
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
