package hubserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBillingMaintenanceCreditStepAdvancesOneTenantEvent(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{OrganizationID: org, Mode: billing.ModeTest, Status: "free"})
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []string{"first", "second"} {
				if _, err := w.CreateBillingEvent(t.Context(), billingstore.Event{OrganizationID: org, ID: id, Type: "customer.updated", ReceivedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{database: &database{billingRepository: repository, hostedOrganization: "org_unselected"}, config: Config{now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		tenant := BillingTenant{Tenant: hubmaintenance.Tenant{Organization: string(org), Generation: 1}, Revision: "runtime_" + string(org), Config: &hostedplans.HostedBillingConfig{Provider: &maintenanceBillingProvider{org: string(org)}}}
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), tenant.Tenant)
		mismatch := tenant
		mismatch.Tenant.Generation++
		if _, _, err := service.MaintainBillingCredit(ctx, mismatch, 0); err == nil {
			t.Fatal("stale allocation generation accepted")
		}
		next, processed, err := service.MaintainBillingCredit(ctx, tenant, 0)
		if err != nil || !processed || next <= 0 {
			t.Fatalf("one-event credit: next=%d processed=%t err=%v", next, processed, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			first, err := r.BillingEvent(t.Context(), "first")
			if err != nil {
				return err
			}
			second, err := r.BillingEvent(t.Context(), "second")
			if err == nil && (!first.CreditProcessed || second.CreditProcessed || first.ProcessedAt != nil || second.ProcessedAt != nil) {
				return errors.New("credit maintenance exceeded one event or marked subscription receipt")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if cursor, processed, err := service.MaintainBillingCredit(ctx, tenant, next); err != nil || !processed || cursor <= next {
			t.Fatalf("credit cursor did not advance: next=%d processed=%t err=%v", cursor, processed, err)
		}
	}
}

type maintenanceBillingProvider struct {
	billing.Provider
	calls int
	org   string
}

type maintenanceTenantRegistry []hubmaintenance.Tenant

func (r maintenanceTenantRegistry) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	var page []hubmaintenance.Tenant
	for _, tenant := range r {
		if tenant.Organization > after && len(page) < limit {
			page = append(page, tenant)
		}
	}
	return page, nil
}

func (p *maintenanceBillingProvider) Reconcile(_ context.Context, binding billing.Binding) (billing.Snapshot, error) {
	p.calls++
	if binding.OrganizationID != p.org {
		return billing.Snapshot{}, errors.New("cross-tenant billing binding")
	}
	return billing.Snapshot{Status: "free"}, nil
}

func TestBillingMaintenanceSubscriptionReceiptsAndDueCheck(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service := &Service{database: &database{billingRepository: repository, hostedOrganization: "org_unselected", billingAuditStore: &memoryRunDetails{}}, config: Config{now: func() time.Time { return now }}}
	providers := make(map[string]*maintenanceBillingProvider)
	tenants := make(map[string]BillingTenant)
	var registry maintenanceTenantRegistry
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		account := "acct_" + string(org)
		seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{OrganizationID: org, Mode: billing.ModeTest, Status: "free", Binding: billing.Binding{OrganizationID: string(org), AccountID: account, CustomerID: "cus_fixture"}})
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			plan := billingstore.PlanReference{ID: "starter", Version: 1}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: org, PlanReference: plan, Name: "Starter", Features: []string{}, Allowances: map[string]int64{}}); err != nil {
				return err
			}
			if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: org, Base: plan}, 0); err != nil {
				return err
			}
			for _, id := range []string{"first", "second"} {
				if _, err := w.CreateBillingEvent(t.Context(), billingstore.Event{OrganizationID: org, ID: id, Type: "customer.updated", ReceivedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		provider := &maintenanceBillingProvider{org: string(org)}
		providers[string(org)] = provider
		tenant := hubmaintenance.Tenant{Organization: string(org), Generation: int64(len(registry) + 1)}
		registry = append(registry, tenant)
		tenants[string(org)] = BillingTenant{Tenant: tenant, Revision: "runtime_" + string(org), Config: &hostedplans.HostedBillingConfig{AccountID: account, ReconcileSeconds: 60, Provider: provider}}
	}
	scheduler, err := hubmaintenance.New(registry, []hubmaintenance.Feature{{Name: "billing", Bytes: 1 << 20, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) error {
		return service.MaintainBillingSubscription(ctx, tenants[tenant.Organization])
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for visit := range 3 {
		if err := scheduler.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			provider := providers[string(org)]
			if provider.calls != min(visit+1, 2) {
				t.Fatalf("provider calls=%d visit=%d", provider.calls, visit)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				pending, err := r.UnprocessedBillingEvents(t.Context(), 3)
				if err == nil && len(pending) != max(0, 1-visit) {
					return errors.New("subscription maintenance exceeded one receipt")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func seedHostedBillingRecordAccount(t *testing.T, repository tenantstore.Repository, org tracker.OrganizationID, account billingstore.Account) {
	t.Helper()
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", CreatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}); err != nil {
			return err
		}
		return writer.SetBillingAccount(t.Context(), account)
	}); err != nil {
		t.Fatal(err)
	}
}
