package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type entryRuntime struct {
	Update    hubserver.HostedRuntimeUpdate
	Prices    []hubserver.HostedBillingPrice
	Configure func(TenantSpec) ([]byte, error)
	File      os.FileInfo
}

func (s *Service) allocationPrices() []hubserver.HostedBillingPrice {
	if state := s.currentRuntime(); state != nil {
		return state.Prices
	}
	if s.config.Allocation == nil {
		return nil
	}
	return s.config.Allocation.Prices
}

func (s *Service) configureRuntimeLauncher() {
	if s.config.Allocation == nil {
		return
	}
	launcher, ok := s.config.Allocation.Launcher.(*ExecLauncher)
	if !ok {
		return
	}
	initial := launcher.Configure
	launcher.Configure = func(spec TenantSpec) ([]byte, error) {
		if err := s.linkRuntimePublication(spec.Directory); err != nil {
			return nil, err
		}
		if state := s.currentRuntime(); state != nil {
			return state.Configure(spec)
		}
		return initial(spec)
	}
}

func (s *Service) reloadConfiguration(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	next, err := s.config.Reload()
	if err != nil {
		return fmt.Errorf("validate cloud file: %w", err)
	}
	if err := next.validate(); err != nil {
		return fmt.Errorf("validate cloud configuration: %w", err)
	}
	s.config.Logger.WarnContext(ctx, "cloud configuration settings require restart; retaining running values", "actor", "operator", "reason", "SIGHUP", "changed", !bytes.Equal(next.RestartSettings, s.config.RestartSettings), "settings", []string{"public_url", "listen", "state_directory", "assertion", "workos", "platform", "staff_emails", "support_actors", "entitlement_administrators", "attachments", "billing", "allocation.paths_and_capacity", "allocation.entitlement_credentials", "allocation.billing_except_prices"})
	if s.config.Allocation == nil || next.Allocation == nil || next.Runtime == nil {
		return errors.New("changing allocation availability requires a restart")
	}
	if s.config.ConfigureRuntime == nil {
		return errors.New("runtime reload requires the configured tenant launcher")
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return err
	}
	update := *next.Runtime
	update.Reason = "SIGHUP cloud configuration reload"
	update.ValidateOnly = false
	update.StageID = uuid.NewString()
	publication := hubserver.HostedRuntimePublication{Tenants: make(map[string]hubserver.HostedRuntimeUpdate)}
	if raw, err := os.ReadFile(s.runtimePublicationPath()); err == nil {
		if err := json.Unmarshal(raw, &publication); err != nil {
			return fmt.Errorf("read runtime publication: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	publication.Update = update
	var tenants []Organization
	for _, organization := range organizations {
		if organization.Managed && organization.State == "ready" {
			tenants = append(tenants, organization)
		}
	}
	for _, tenant := range tenants {
		validation := update
		validation.ValidateOnly = true
		if err := s.changeTenantRuntime(ctx, tenant, &validation); err != nil {
			return err
		}
	}
	for _, tenant := range tenants {
		if err := s.linkRuntimePublication(s.tenantSpec(tenant).Directory); err != nil {
			return err
		}
		prepared := update
		if err := s.changeTenantRuntime(ctx, tenant, &prepared); err != nil {
			return err
		}
		publication.Tenants[tenant.ID] = prepared
	}
	publication.Published = append(slices.Clone(publication.Published), update.StageID)
	raw, err := json.Marshal(publication)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writePrivateFile(s.runtimePublicationPath(), raw); err != nil {
		return fmt.Errorf("publish runtime settings: %w", err)
	}
	s.currentRuntime()
	s.allowlist.Store(&signupAllowlist{emails: slices.Clone(next.Allocation.AllowedEmails), domains: slices.Clone(next.Allocation.AllowedDomains)})
	s.config.Logger.InfoContext(ctx, "cloud configuration reloaded", "actor", "operator", "reason", "SIGHUP", "settings", []string{"allocation.entitlements", "allocation.billing.prices", "allocation.allowlist"})
	return nil
}

func (s *Service) runtimePublicationPath() string {
	return filepath.Join(s.config.StateDir, hubserver.HostedRuntimeFilename)
}

func (s *Service) publishTenantRuntime(ctx context.Context, organization Organization) error {
	raw, err := os.ReadFile(s.runtimePublicationPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	publication := hubserver.HostedRuntimePublication{Tenants: make(map[string]hubserver.HostedRuntimeUpdate)}
	if err := json.Unmarshal(raw, &publication); err != nil {
		return fmt.Errorf("read runtime publication: %w", err)
	}
	if err := s.linkRuntimePublication(s.tenantSpec(organization).Directory); err != nil {
		return err
	}
	update := publication.Update
	update.Reason = "tenant provisioning adopts published cloud runtime"
	update.StageID = uuid.NewString()
	if err := s.changeTenantRuntime(ctx, organization, &update); err != nil {
		return err
	}
	publication.Tenants[organization.ID] = update
	publication.Published = append(slices.Clone(publication.Published), update.StageID)
	raw, err = json.Marshal(publication)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writePrivateFile(s.runtimePublicationPath(), raw); err != nil {
		return fmt.Errorf("publish tenant runtime settings: %w", err)
	}
	return nil
}

func (s *Service) linkRuntimePublication(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, hubserver.HostedRuntimeFilename)
	target, err := filepath.Abs(s.runtimePublicationPath())
	if err != nil {
		return err
	}
	if existing, err := os.Readlink(path); err == nil {
		if existing != target {
			return errors.New("tenant runtime publication belongs to a different entry")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, path)
}

func (s *Service) currentRuntime() *entryRuntime {
	current := s.runtime.Load()
	file, err := os.Stat(s.runtimePublicationPath())
	if err != nil || current != nil && current.File != nil && os.SameFile(file, current.File) && file.ModTime().Equal(current.File.ModTime()) && file.Size() == current.File.Size() {
		return current
	}
	raw, err := os.ReadFile(s.runtimePublicationPath())
	if err != nil {
		return current
	}
	var publication hubserver.HostedRuntimePublication
	if json.Unmarshal(raw, &publication) != nil || s.config.ConfigureRuntime == nil {
		return current
	}
	state := &entryRuntime{Update: publication.Update, Prices: publication.Update.Prices, Configure: s.config.ConfigureRuntime(publication.Update), File: file}
	s.runtime.Store(state)
	return state
}

func (s *Service) changeTenantRuntime(ctx context.Context, organization Organization, update *hubserver.HostedRuntimeUpdate) error {
	body, err := json.Marshal(update)
	if err != nil {
		return err
	}
	status, response, err := s.machineCall(ctx, organization, http.MethodPost, tenantEntitlementsPath(organization.ID)+"/runtime", body, string(s.config.Allocation.EntitlementAdminToken))
	if err != nil {
		return fmt.Errorf("tenant %s runtime reload: %w", organization.ID, err)
	}
	if status == http.StatusOK && update.StageID != "" && !update.ValidateOnly {
		if err := json.Unmarshal(response, update); err != nil {
			return fmt.Errorf("tenant %s runtime preparation: %w", organization.ID, err)
		}
		return nil
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("tenant %s runtime reload answered %d", organization.ID, status)
	}
	return nil
}
