package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type entryRuntime struct {
	Update    hubserver.HostedRuntimeUpdate
	Prices    []hubserver.HostedBillingPrice
	Configure func(TenantSpec) ([]byte, error)
}

func (s *Service) allocationPrices() []hubserver.HostedBillingPrice {
	if state := s.runtime.Load(); state != nil {
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
		if state := s.runtime.Load(); state != nil {
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
	s.config.Logger.WarnContext(ctx, "cloud configuration settings require restart; retaining running values", "actor", "operator", "reason", "SIGHUP", "changed", !bytes.Equal(next.RestartSettings, s.config.RestartSettings), "settings", []string{"public_url", "listen", "state_directory", "assertion", "workos", "platform", "staff_emails", "support_actors", "entitlement_administrators", "attachments", "billing", "allocation.paths_and_capacity", "allocation.allowlist", "allocation.entitlement_credentials", "allocation.billing_except_prices"})
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
	for _, validate := range []bool{true, false} {
		update.ValidateOnly = validate
		body, err := json.Marshal(update)
		if err != nil {
			return err
		}
		for _, organization := range organizations {
			if !organization.Managed || organization.State != "ready" {
				continue
			}
			status, _, err := s.machineCall(ctx, organization, http.MethodPost, tenantEntitlementsPath(organization.ID)+"/runtime", body, string(s.config.Allocation.EntitlementAdminToken))
			if err != nil {
				return fmt.Errorf("tenant runtime reload: %w", err)
			}
			if status != http.StatusNoContent {
				return fmt.Errorf("tenant runtime reload answered %d", status)
			}
		}
	}
	s.runtime.Store(&entryRuntime{Update: update, Prices: next.Allocation.Prices, Configure: s.config.ConfigureRuntime(update)})
	s.config.Logger.InfoContext(ctx, "cloud configuration reloaded", "actor", "operator", "reason", "SIGHUP", "settings", []string{"allocation.entitlements", "allocation.billing.prices"})
	return nil
}
