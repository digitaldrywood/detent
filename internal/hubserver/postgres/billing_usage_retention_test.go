package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRequestBillingRetentionIsBoundedAndTenantScoped(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	cutoff := historyAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for i, metric := range []string{"http_requests", "http_response_bytes", "ai_calls", "http_requests"} {
				at := cutoff
				if i == 3 {
					at = at.Add(time.Minute)
				}
				if _, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: org, ID: fmt.Sprintf("receipt_%d", i), WindowStart: at, Metric: metric, Amount: 1}, 100); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ limit, remaining int }{{1, 3}, {128, 2}} {
		if err := repository.WriteTenant(t.Context(), "org_first", corestore.Background, func(w tenantstore.Writer) error { return w.PruneRequestBillingUsage(t.Context(), cutoff, test.limit) }); err != nil {
			t.Fatal(err)
		}
		for _, org := range []string{"org_first", "org_second"} {
			want := test.remaining
			if org == "org_second" {
				want = 4
			}
			for _, query := range []string{`SELECT count(*) FROM hosted_usage_receipts WHERE organization_id=$1`, `SELECT count(*) FROM hosted_usage_windows WHERE organization_id=$1`} {
				var got int
				if err := admin.QueryRowContext(t.Context(), query, org).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != want {
					t.Fatalf("organization=%s retained=%d want=%d", org, got, want)
				}
			}
		}
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		for _, row := range []billingstore.Usage{{WindowStart: cutoff, Metric: "ai_calls"}, {WindowStart: cutoff.Add(time.Minute), Metric: "http_requests"}} {
			got, err := r.BillingUsage(t.Context(), row.WindowStart, row.Metric)
			if err != nil || got != 1 {
				t.Fatalf("retained %s=%d error=%v", row.Metric, got, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
