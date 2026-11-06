package cloudentry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hubserver"
)

type entryRuntime struct {
	Update         hubserver.HostedRuntimeUpdate
	Prices         []hubserver.HostedBillingPrice
	Configure      func(TenantSpec) ([]byte, error)
	Revision       string
	AllowedEmails  []string
	AllowedDomains []string
}

type RuntimeAdopter interface {
	AdoptRuntime(context.Context, Organization, string, hubserver.HostedRuntimeUpdate) error
}

type entryRuntimeObject struct {
	hubserver.HostedRuntimeUpdate
	AllowedEmails  []string `json:"allowed_emails"`
	AllowedDomains []string `json:"allowed_domains"`
}

func (s *Service) runtimeAdopter() (RuntimeAdopter, bool) {
	if s.config.Allocation == nil {
		return nil, false
	}
	adopter, ok := s.config.Allocation.Launcher.(RuntimeAdopter)
	return adopter, ok
}

func (s *Service) RuntimeTenantPage(ctx context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	return s.registry.store.db.postgres.RuntimeTenantPage(ctx, after, limit)
}

func (s *Service) AdoptPublishedRuntime(ctx context.Context, tenant hubmaintenance.Tenant) error {
	adopter, ok := s.runtimeAdopter()
	if !ok {
		return errors.New("shared runtime adoption is unavailable")
	}
	target, err := s.registry.store.db.postgres.RuntimeAdoption(ctx, tracker.OrganizationID(tenant.Organization), s.config.Issuer)
	if err != nil {
		return err
	}
	if target.Generation != tenant.Generation {
		return corestore.ErrRevisionConflict
	}
	if !target.Managed || target.State != "ready" && target.State != "allocating" || target.PreparedRevision == target.Publication.Revision {
		return nil
	}
	raw, err := s.readEntryObject(ctx, target.Publication.ObjectKey)
	if err != nil {
		return err
	}
	var payload entryRuntimeObject
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	if payload.StageID != target.Publication.Revision {
		return corestore.ErrRevisionConflict
	}
	if err := hubserver.ValidateHostedRuntimeUpdate(payload.HostedRuntimeUpdate); err != nil {
		return err
	}
	organization := Organization{ID: tenant.Organization, Generation: tenant.Generation, Managed: target.Managed, State: target.State}
	return adopter.AdoptRuntime(ctx, organization, target.PreparedRevision, payload.HostedRuntimeUpdate)
}

func (s *Service) allocationPrices(ctx context.Context) []hubserver.HostedBillingPrice {
	if state := s.currentRuntime(ctx); state != nil {
		return state.Prices
	}
	if s.config.Allocation == nil {
		return nil
	}
	return s.config.Allocation.Prices
}

func (s *Service) configureRuntimeLauncher(ctx context.Context) {
	if s.config.Allocation == nil {
		return
	}
	launcher, ok := s.config.Allocation.Launcher.(*ExecLauncher)
	if !ok {
		return
	}
	initial := launcher.Configure
	launcher.Configure = func(spec TenantSpec) ([]byte, error) {
		lookup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		state, err := s.loadRuntime(lookup)
		cancel()
		if err != nil {
			return nil, err
		}
		if state != nil {
			return state.Configure(spec)
		}
		return initial(spec)
	}
}

func (s *Service) reloadConfiguration(ctx context.Context) error {
	tenants, err := s.publishRuntimeConfiguration(ctx)
	s.refreshPublishedTenants(ctx, tenants)
	if err != nil {
		return err
	}
	s.config.Logger.InfoContext(ctx, "cloud configuration reloaded", "actor", "operator", "reason", "SIGHUP", "settings", []string{"allocation.entitlements", "allocation.billing.prices", "allocation.allowlist"})
	return nil
}

func (s *Service) publishRuntimeConfiguration(ctx context.Context) ([]Organization, error) {
	next, err := s.config.Reload()
	if err != nil {
		return nil, fmt.Errorf("validate cloud file: %w", err)
	}
	if err := next.validate(); err != nil {
		return nil, fmt.Errorf("validate cloud configuration: %w", err)
	}
	s.config.Logger.WarnContext(ctx, "cloud configuration settings require restart; retaining running values", "actor", "operator", "reason", "SIGHUP", "changed", !bytes.Equal(next.RestartSettings, s.config.RestartSettings), "settings", []string{"public_url", "listen", "state_directory", "assertion", "workos", "platform", "staff_emails", "support_actors", "entitlement_administrators", "attachments", "billing", "allocation.paths_and_capacity", "allocation.entitlement_credentials", "allocation.billing_except_prices"})
	if s.config.Allocation == nil || next.Allocation == nil || next.Runtime == nil {
		return nil, errors.New("changing allocation availability requires a restart")
	}
	if _, shared := s.runtimeAdopter(); shared {
		return nil, s.publishSharedRuntimeConfiguration(ctx, next)
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	if s.config.ConfigureRuntime == nil {
		return nil, errors.New("runtime reload requires the configured tenant launcher")
	}
	organizations, err := s.registry.List(ctx)
	if err != nil {
		return nil, err
	}
	update := *next.Runtime
	update.Reason = "SIGHUP cloud configuration reload"
	update.ValidateOnly = false
	update.StageID = uuid.NewString()
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
			return nil, err
		}
	}
	for _, tenant := range tenants {
		prepared := update
		if err := s.changeTenantRuntime(ctx, tenant, &prepared); err != nil {
			return nil, err
		}

	}
	raw, err := json.Marshal(update)
	if err != nil {
		return nil, err
	}
	key, err := s.storeEntryObject(ctx, raw)
	if err != nil {
		return nil, err
	}
	targets := make([]tracker.OrganizationID, len(tenants))
	for i, tenant := range tenants {
		targets[i] = tracker.OrganizationID(tenant.ID)
	}
	if err := s.registry.store.db.postgres.PublishRuntime(ctx, update.StageID, s.config.Issuer, key, targets); err != nil {
		return nil, fmt.Errorf("publish runtime settings: %w", err)
	}
	if _, err := s.loadRuntime(ctx); err != nil {
		return tenants, err
	}
	s.allowlist.Store(&signupAllowlist{emails: slices.Clone(next.Allocation.AllowedEmails), domains: slices.Clone(next.Allocation.AllowedDomains)})
	return tenants, nil
}

func (s *Service) publishSharedRuntimeConfiguration(ctx context.Context, next Config) error {
	update := *next.Runtime
	update.Reason = "SIGHUP cloud configuration reload"
	update.ValidateOnly = false
	update.StageID = uuid.NewString()
	if update.Plans == nil {
		update.Plans = &hubserver.HostedPlansConfig{}
	}
	if err := hubserver.ValidateHostedRuntimeUpdate(update); err != nil {
		return err
	}
	raw, err := json.Marshal(entryRuntimeObject{HostedRuntimeUpdate: update, AllowedEmails: next.Allocation.AllowedEmails, AllowedDomains: next.Allocation.AllowedDomains})
	if err != nil {
		return err
	}
	key, err := s.storeEntryObject(ctx, raw)
	if err != nil {
		return err
	}
	s.reloadMu.Lock()
	err = s.registry.store.db.postgres.PublishRuntime(ctx, update.StageID, s.config.Issuer, key, nil)
	s.reloadMu.Unlock()
	if err != nil {
		return err
	}
	_, err = s.loadRuntime(ctx)
	return err
}

func (s *Service) publishTenantRuntime(ctx context.Context, organization Organization) error {
	if _, shared := s.runtimeAdopter(); shared {
		return s.AdoptPublishedRuntime(ctx, hubmaintenance.Tenant{Organization: organization.ID, Generation: organization.Generation})
	}
	state, err := s.loadRuntime(ctx)
	if err != nil || state == nil {
		return err
	}
	update := state.Update
	update.Reason = "tenant provisioning adopts published cloud runtime"
	update.StageID = uuid.NewString()
	if err := s.changeTenantRuntime(ctx, organization, &update); err != nil {
		return err
	}
	if err := s.registry.store.db.postgres.PublishRuntime(ctx, update.StageID, "", "", []tracker.OrganizationID{tracker.OrganizationID(organization.ID)}); err != nil {
		return fmt.Errorf("publish tenant runtime settings: %w", err)
	}
	s.refreshPublishedTenants(ctx, []Organization{organization})
	return nil
}

func (s *Service) loadRuntime(ctx context.Context) (*entryRuntime, error) {
	current := s.runtime.Load()
	if s.registry == nil {
		return current, nil
	}
	publication, err := s.registry.store.db.postgres.RuntimePublication(ctx, s.config.Issuer)
	if errors.Is(err, corestore.ErrNotFound) {
		if _, shared := s.runtimeAdopter(); shared {
			return nil, err
		}
		return current, nil
	}
	if err != nil {
		return nil, err
	}
	if current != nil && current.Revision == publication.Revision {
		return current, nil
	}
	raw, err := s.readEntryObject(ctx, publication.ObjectKey)
	if err != nil {
		return nil, err
	}
	var payload entryRuntimeObject
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	update := payload.HostedRuntimeUpdate
	_, shared := s.runtimeAdopter()
	if shared {
		if update.StageID != publication.Revision {
			return nil, corestore.ErrRevisionConflict
		}
		if err := hubserver.ValidateHostedRuntimeUpdate(update); err != nil {
			return nil, err
		}
	}
	if s.config.ConfigureRuntime == nil && !shared {
		return nil, errors.New("runtime reload requires the configured tenant launcher")
	}
	state := &entryRuntime{Update: update, Prices: update.Prices, Revision: publication.Revision, AllowedEmails: slices.Clone(payload.AllowedEmails), AllowedDomains: slices.Clone(payload.AllowedDomains)}
	if s.config.ConfigureRuntime != nil {
		state.Configure = s.config.ConfigureRuntime(update)
	}
	if shared {
		s.reloadMu.Lock()
		defer s.reloadMu.Unlock()
	}
	if s.runtime.CompareAndSwap(current, state) {
		if shared {
			s.allowlist.Store(&signupAllowlist{emails: state.AllowedEmails, domains: state.AllowedDomains})
		}
		return state, nil
	}
	return s.runtime.Load(), nil
}

func (s *Service) currentRuntime(ctx context.Context) *entryRuntime {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state, err := s.loadRuntime(ctx)
	if err != nil {
		return s.runtime.Load()
	}
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

func (s *Service) refreshPublishedTenants(ctx context.Context, tenants []Organization) {
	for i, tenant := range tenants {
		if i > 0 && len(tenants) > 32 {
			jitter := sha256.Sum256([]byte(tenant.ID))
			timer := time.NewTimer(time.Duration(1+int(jitter[0])%10) * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
		if err := ctx.Err(); err != nil {
			s.config.Logger.WarnContext(ctx, "published runtime refresh interrupted", "remaining", len(tenants)-i, "error", err)
			return
		}
		status, _, err := s.machineCall(ctx, tenant, http.MethodGet, tenantEntitlementsPath(tenant.ID)+"/runtime", nil, string(s.config.Allocation.EntitlementAdminToken))
		if err != nil || status != http.StatusOK {
			s.config.Logger.WarnContext(ctx, "published runtime tenant refresh failed", "organization", tenant.ID, "status", status, "error", err)
		}
	}
}
