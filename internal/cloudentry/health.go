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

func (s *Service) waitForTenantReadiness(ctx context.Context) error {
	if s.config.Allocation == nil {
		return nil
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return fmt.Errorf("list tenant Hubs for startup: %w", err)
	}
	for _, organization := range organizations {
		if !organization.Managed || organization.State != "ready" {
			continue
		}
		if err := s.config.Allocation.Launcher.WaitReady(ctx, organization.ID); err != nil {
			return fmt.Errorf("wait for tenant Hub readiness: %w", err)
		}
	}
	return nil
}

type platformTenantHealth struct {
	migration     hubserver.MigrationHealth
	schemaKnown   bool
	schemaSet     string
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
		row.schemaSet = row.migration.SchemaFingerprint
		row.schemaKnown = err == nil && row.migration.SchemaFactsComplete && row.migration.SchemaVersion > 0 && row.schemaSet != "" && !row.migration.MigrationsRunning
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
	ctx := context.WithValue(c.Request().Context(), entryHealthReadKey{}, true)
	organizations, err := s.registry.List(ctx)
	status, code := "unavailable", http.StatusServiceUnavailable
	var migration hubserver.MigrationHealth
	var registrySchema, authSchema, platformSchema int64
	var registryFingerprint, authFingerprint, platformFingerprint string
	registryApplicable, authApplicable := s.config.Storage.Backend != "postgres", s.config.Storage.Backend != "postgres"
	platformApplicable := s.config.Storage.Backend == "postgres"
	hubApplicable := false
	readyOrganizations := 0
	var factsComplete bool
	if err == nil && s.auth.store.db.PingContext(ctx) == nil {
		factsComplete = true
		for _, organization := range organizations {
			switch organization.State {
			case "ready":
				hubApplicable = true
				readyOrganizations++
			case "requested", "allocating":
				hubApplicable = true
				factsComplete = false
			case "failed", "disabled", "deleting", "deleted":
			default:
				hubApplicable = true
				factsComplete = false
			}
		}
		rows := s.tenantHealth(ctx, organizations)
		running := 0
		var hubFingerprint string
		for _, row := range rows {
			if row.Running {
				running++
			}
			migration.SchemaVersion = max(migration.SchemaVersion, row.migration.SchemaVersion)
			migration.MigrationsRunning = migration.MigrationsRunning || row.migration.MigrationsRunning
			factsComplete = factsComplete && row.schemaKnown
			if row.schemaKnown {
				if hubFingerprint == "" {
					hubFingerprint = row.schemaSet
				} else if hubFingerprint != row.schemaSet {
					factsComplete = false
				}
			}
		}
		migration.SchemaFingerprint = hubFingerprint
		factsComplete = factsComplete && len(rows) == readyOrganizations
		if running == len(rows) {
			status, code = "ok", http.StatusOK
		}
		if registryApplicable {
			registrySchema, err = s.registry.store.schemaVersion(ctx)
			if err == nil {
				registryFingerprint, err = s.registry.store.schemaFingerprint(ctx)
			}
			if err == nil {
				authSchema, err = s.auth.store.schemaVersion(ctx)
			}
			if err == nil {
				authFingerprint, err = s.auth.store.schemaFingerprint(ctx)
			}
		} else {
			platformSchema, err = s.registry.store.schemaVersion(ctx)
			if err == nil {
				platformFingerprint, err = s.registry.store.schemaFingerprint(ctx)
			}
		}
		if err != nil {
			status, code = "unavailable", http.StatusServiceUnavailable
			factsComplete = false
		}
	} else {
		factsComplete = false
	}
	return c.JSON(code, struct {
		hubserver.MigrationHealth
		HubSchemaApplicable       bool   `json:"hub_schema_applicable"`
		RegistrySchemaVersion     int64  `json:"registry_schema_version"`
		RegistrySchemaFingerprint string `json:"registry_schema_fingerprint"`
		RegistrySchemaApplicable  bool   `json:"registry_schema_applicable"`
		AuthSchemaVersion         int64  `json:"auth_schema_version"`
		AuthSchemaFingerprint     string `json:"auth_schema_fingerprint"`
		AuthSchemaApplicable      bool   `json:"auth_schema_applicable"`
		PlatformSchemaVersion     int64  `json:"platform_schema_version"`
		PlatformSchemaFingerprint string `json:"platform_schema_fingerprint"`
		PlatformSchemaApplicable  bool   `json:"platform_schema_applicable"`
		SchemaFactsComplete       bool   `json:"schema_facts_complete"`
		Status                    string `json:"status"`
		Version                   string `json:"version"`
		Commit                    string `json:"commit"`
	}{migration, hubApplicable, registrySchema, registryFingerprint, registryApplicable, authSchema, authFingerprint, authApplicable, platformSchema, platformFingerprint, platformApplicable, factsComplete, status, s.config.Build.Version, s.config.Build.Commit})
}
