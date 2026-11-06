package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConfigureHostedBillingRecordsPreservesPaidAccount(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	d := &database{billingRepository: repository, hostedOrganization: "org_example", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
	org := tracker.OrganizationID(d.hostedOrganization)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{
		OrganizationID: org,
		Mode:           billing.ModeTest,
		Binding:        billing.Binding{OrganizationID: string(org), AccountID: "acct_fixture", CustomerID: "cus_fixture"},
		Snapshot:       billing.Snapshot{SubscriptionID: "sub_paid", PriceID: "price_paid", Status: "active"},
		Status:         "active",
		ReconciledAt:   &now,
		Checkout:       billingstore.Checkout{Key: "checkout_fixture", PriceID: "price_paid", Session: billing.Session{ID: "cs_fixture", URL: "https://billing.example.test/session"}},
	})
	if err := d.configureHostedBillingRecords(t.Context(), &HostedBillingConfig{Mode: billing.ModeTest, AccountID: "acct_fixture", CustomerID: "cus_fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(t.Context())
		if err != nil {
			return err
		}
		if account.Status != "active" || account.Snapshot.SubscriptionID != "sub_paid" || account.Snapshot.PriceID != "price_paid" || account.ReconciledAt == nil || !account.ReconciledAt.Equal(now) || account.Checkout.Session.ID != "cs_fixture" {
			return errors.New("hosted billing configuration replaced paid account state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBillingMaintenanceCreditStepAdvancesOneTenantEvent(t *testing.T) {
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
		tenant := BillingTenant{Tenant: hubmaintenance.Tenant{Organization: string(org), Generation: 1}, Revision: "runtime_" + string(org), Config: &HostedBillingConfig{Provider: &maintenanceBillingProvider{org: string(org)}}}
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
		tenants[string(org)] = BillingTenant{Tenant: tenant, Revision: "runtime_" + string(org), Config: &HostedBillingConfig{AccountID: account, ReconcileSeconds: 60, Provider: provider}}
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

func TestConfigureHostedBillingRecordsPreservesConcurrentPaidInsert(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	d := &database{billingRepository: repository, hostedOrganization: "org_example", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
	org := tracker.OrganizationID(d.hostedOrganization)
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
		return writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", CreatedAt: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)})
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var insertErr error
	d.now = func() time.Time {
		if insertErr == nil {
			insertErr = repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
				return writer.SetBillingAccount(t.Context(), billingstore.Account{
					OrganizationID: org,
					Mode:           billing.ModeTest,
					Binding:        billing.Binding{OrganizationID: string(org), AccountID: "acct_fixture", CustomerID: "cus_fixture"},
					Snapshot:       billing.Snapshot{SubscriptionID: "sub_concurrent", Status: "active"},
					Status:         "active",
				})
			})
		}
		return now
	}
	if err := d.configureHostedBillingRecords(t.Context(), &HostedBillingConfig{Mode: billing.ModeTest, AccountID: "acct_fixture", CustomerID: "cus_fixture"}); err != nil {
		t.Fatal(err)
	}
	if insertErr != nil {
		t.Fatalf("concurrent account insert: %v", insertErr)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(t.Context())
		if err != nil {
			return err
		}
		if account.Status != "active" || account.Snapshot.SubscriptionID != "sub_concurrent" {
			return errors.New("configuration replaced a concurrent paid account")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureHostedBillingRecordsRejectsChangedBindingBeforeRetire(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	d := &database{billingRepository: repository, hostedOrganization: "org_example", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
	org := tracker.OrganizationID(d.hostedOrganization)
	seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{
		OrganizationID: org,
		Mode:           billing.ModeTest,
		Binding:        billing.Binding{OrganizationID: string(org), AccountID: "acct_fixture", CustomerID: "cus_fixture"},
		Status:         "active",
	})
	d.billingAuditStore = hostedBillingRetireInterference{repository: repository, organization: org, mutate: true}
	config := &HostedBillingConfig{Mode: billing.ModeLive, AccountID: "acct_fixture"}
	if err := d.configureHostedBillingRecords(t.Context(), config); err == nil {
		t.Fatal("retired a billing binding changed after its archived snapshot")
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(t.Context())
		if err != nil {
			return err
		}
		if account.Mode != billing.ModeTest || account.Binding.CustomerID != "cus_changed" {
			return errors.New("changed billing binding was deleted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureHostedBillingRecordsRetiresUnchangedTestBinding(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	d := &database{billingRepository: repository, hostedOrganization: "org_example", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
	org := tracker.OrganizationID(d.hostedOrganization)
	seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{
		OrganizationID: org,
		Mode:           billing.ModeTest,
		Binding:        billing.Binding{OrganizationID: string(org), AccountID: "acct_fixture", CustomerID: "cus_fixture"},
		Status:         "active",
	})
	d.billingAuditStore = hostedBillingRetireInterference{repository: repository, organization: org}
	if err := d.configureHostedBillingRecords(t.Context(), &HostedBillingConfig{Mode: billing.ModeLive, AccountID: "acct_fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		_, err := reader.BillingAccount(t.Context())
		if !errors.Is(err, corestore.ErrNotFound) {
			return errors.New("retired test account remains active")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCommitHostedBillingRecordsUpdatesReconcileState(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	d := &database{billingRepository: repository, hostedOrganization: "org_example", billingAuditStore: &memoryRunDetails{}, now: time.Now, logger: discardLogger()}
	org := tracker.OrganizationID(d.hostedOrganization)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	seedHostedBillingRecordAccount(t, repository, org, billingstore.Account{
		OrganizationID: org,
		Mode:           billing.ModeTest,
		Binding:        billing.Binding{OrganizationID: string(org), AccountID: "acct_fixture", CustomerID: "cus_fixture"},
		Status:         "free",
		Checkout:       billingstore.Checkout{Key: "checkout_fixture", PriceID: "price_fixture", Session: billing.Session{ID: "cs_fixture", URL: "https://billing.example.test/session"}},
	})
	basePlan := billingstore.PlanReference{ID: "starter", Version: 1}
	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: org, PlanReference: basePlan, Name: "Starter", Features: []string{}, Allowances: map[string]int64{}}); err != nil {
			return err
		}
		return writer.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: org, Base: basePlan}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	d.billingAuditStore = hostedBillingRetireInterference{repository: repository, organization: org}
	state := hostedBillingState{Status: "active", Snapshot: billing.Snapshot{SubscriptionID: "sub_reconciled", Status: "active"}}
	if err := d.commitHostedBilling(t.Context(), state, 0, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(t.Context())
		if err != nil {
			return err
		}
		if account.ReconciledAt == nil || !account.ReconciledAt.Equal(now) || account.Snapshot.SubscriptionID != "sub_reconciled" || account.Checkout.Session.ID != "" {
			return errors.New("reconciliation did not update timestamp or clear completed checkout")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
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

type hostedBillingRetireInterference struct {
	repository   tenantstore.Repository
	organization tracker.OrganizationID
	mutate       bool
}

func (s hostedBillingRetireInterference) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	if s.mutate {
		err := s.repository.WriteTenant(ctx, s.organization, corestore.Serving, func(writer tenantstore.Writer) error {
			account, err := writer.BillingAccount(ctx)
			if err != nil {
				return err
			}
			if err := writer.DeleteBillingAccount(ctx); err != nil {
				return err
			}
			account.Binding.CustomerID = "cus_changed"
			return writer.SetBillingAccount(ctx, account)
		})
		if err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	key, err := attachment.ArchiveKey(organization, digestText)
	if err != nil {
		return attachment.ArchiveReference{}, err
	}
	return attachment.ArchiveReference{Key: key, SHA256: digestText, CompressedSize: 1, UncompressedSize: 1, Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}, nil
}

func (hostedBillingRetireInterference) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, errors.New("unexpected billing detail read")
}
