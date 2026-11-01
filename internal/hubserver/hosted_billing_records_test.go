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
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConfigureHostedBillingRecordsPreservesPaidAccount(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	repository := projectRecordPostgres(t)
	d := f.service.database
	d.billingRepository = repository
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

func TestConfigureHostedBillingRecordsPreservesConcurrentPaidInsert(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	repository := projectRecordPostgres(t)
	d := f.service.database
	d.billingRepository = repository
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
	f := newHostedSharedFixture(t)
	repository := projectRecordPostgres(t)
	d := f.service.database
	d.billingRepository = repository
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
	f := newHostedSharedFixture(t)
	repository := projectRecordPostgres(t)
	d := f.service.database
	d.billingRepository = repository
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
	f := newHostedSharedFixture(t)
	repository := projectRecordPostgres(t)
	d := f.service.database
	d.billingRepository = repository
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
