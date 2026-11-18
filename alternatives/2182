package hubtest_test

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPlanCatalogSeedsEachOrganization(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	for index := range 2 {
		org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
		plan := billingstore.Plan{PlanReference: billingstore.PlanReference{ID: "example", Version: 1}, Name: "Example", MonthlyUSDCents: new(int64(index + 1)), Features: []string{"collaboration"}, Allowances: map[string]int64{"projects": int64(index + 2)}}
		hubtest.PlanCatalog(t, repository, org, []billingstore.Plan{plan})
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := r.BillingPlan(t.Context(), plan.PlanReference)
			if err == nil && (got.OrganizationID != org || got.Name != plan.Name || got.MonthlyUSDCents == nil || *got.MonthlyUSDCents != int64(index+1) || len(got.Features) != 1 || got.Features[0] != "collaboration" || got.Allowances["projects"] != int64(index+2)) {
				t.Fatalf("plan=%+v", got)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlanAssignmentSeedsPaidWindowsPerOrganization(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for index := range 2 {
		org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
		base := billingstore.PlanReference{ID: "base", Version: 1}
		paid := billingstore.PlanReference{ID: "paid", Version: 1}
		hubtest.PlanCatalog(t, repository, org, []billingstore.Plan{{PlanReference: base}, {PlanReference: paid}})
		end := start.AddDate(0, index+1, 0)
		for revision := range 2 {
			seeded := hubtest.PlanAssignment(t, repository, org, hubtest.PlanAssignmentOptions{Base: base, Subscription: &paid, StartsAt: start, EndsAt: end, AccessUntil: end.Add(time.Hour)})
			if seeded.Revision != int64(revision+1) {
				t.Fatalf("seeded revision=%d want=%d", seeded.Revision, revision+1)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			assignment, err := r.BillingAssignment(t.Context())
			if err != nil {
				return err
			}
			if assignment.OrganizationID != org || assignment.Base != base || assignment.Subscription == nil || *assignment.Subscription != paid || assignment.ExpiresAt == nil || !assignment.ExpiresAt.Equal(end.Add(time.Hour)) || assignment.Revision != 2 {
				t.Fatalf("assignment=%+v", assignment)
			}
			account, err := r.BillingAccount(t.Context())
			if err == nil && (account.OrganizationID != org || account.Plan != paid || !account.Snapshot.PeriodStart.Equal(start) || !account.Snapshot.PeriodEnd.Equal(end) || account.Binding.CustomerID != "cus_"+string(org) || !account.AccessUntil.Equal(end.Add(time.Hour))) {
				t.Fatalf("account=%+v", account)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreditGrantSeedsUsableTenantLedgers(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	for index := range 2 {
		org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
		row := hubtest.CreditGrant(t, repository, org, billingstore.CreditTransaction{AmountMicros: int64(index + 1), Source: "same-source"})
		other := hubtest.CreditGrant(t, repository, org, billingstore.CreditTransaction{AmountMicros: 10})
		if other.Source == row.Source {
			t.Fatal("generated credit source collided")
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			account, err := r.BillingCreditAccount(t.Context(), billing.ModeTest)
			if err != nil {
				return err
			}
			if account.OrganizationID != org || account.BalanceMicros != int64(index+11) {
				t.Fatalf("account=%+v", account)
			}
			got, err := r.BillingCreditTransactionBySource(t.Context(), billing.ModeTest, row.Source)
			if err == nil && (got.AmountMicros != int64(index+1) || got.OrganizationID != org || got.Kind != "complimentary" || !got.RecordedAt.Equal(row.RecordedAt)) {
				t.Fatalf("credit=%+v", got)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
