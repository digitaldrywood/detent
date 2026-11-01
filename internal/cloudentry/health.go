package cloudentry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

var errTenantMigrating = errors.New("tenant migrations are running")

func (s *Service) tenantMigrationHealth(ctx context.Context, organization Organization) (hubserver.MigrationHealth, error) {
	status, body, err := s.serviceCall(ctx, organization, "/internal/v1/health", map[string]string{}, nil)
	var health hubserver.MigrationHealth
	if err != nil {
		return health, err
	}
	if status == http.StatusNoContent {
		return health, nil
	}
	if err := json.Unmarshal(body, &health); err != nil {
		return health, fmt.Errorf("decode tenant health: %w", err)
	}
	if status == http.StatusServiceUnavailable && health.MigrationsRunning {
		return health, errTenantMigrating
	}
	if status != http.StatusOK {
		return health, fmt.Errorf("tenant health returned HTTP %d", status)
	}
	return health, nil
}

func (s *Service) checkTenant(ctx context.Context, organization Organization) error {
	_, err := s.tenantMigrationHealth(ctx, organization)
	return err
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
	migration     hubserver.MigrationHealth
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
		var err error
		row.migration, err = s.tenantMigrationHealth(call, organization)
		row.Running = err == nil
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
	var migration hubserver.MigrationHealth
	if err == nil && s.auth.store.db.PingContext(ctx) == nil {
		rows := s.tenantHealth(ctx, organizations)
		running := 0
		for _, row := range rows {
			if row.Running {
				running++
			}
			migration.SchemaVersion = max(migration.SchemaVersion, row.migration.SchemaVersion)
			migration.MigrationsRunning = migration.MigrationsRunning || row.migration.MigrationsRunning
		}
		if running == len(rows) {
			status, code = "ok", http.StatusOK
		}
	}
	return c.JSON(code, struct {
		hubserver.MigrationHealth
		Status  string `json:"status"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}{migration, status, s.config.Build.Version, s.config.Build.Commit})
}
