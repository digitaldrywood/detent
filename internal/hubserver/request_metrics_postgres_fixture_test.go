package hubserver

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func newPostgresTestMetrics(t *testing.T, org tracker.OrganizationID) (*tenantRequestMetrics, error) {
	t.Helper()
	return requestmetrics.New(requestmetrics.Deps{Repository: projectRecordPostgres(t), Organization: org})
}
func newTenantTestMetrics(repo metricsstore.Repository, org tracker.OrganizationID) *tenantRequestMetrics {
	m, err := requestmetrics.New(requestmetrics.Deps{Repository: repo, Organization: org})
	if err != nil {
		panic(err)
	}
	return m
}

func TestScheduledCIClassificationFallbackReceiptRetention(t *testing.T) {
	if os.Getenv(pgtest.Environment) == "" {
		t.Setenv(pgtest.Environment, "binary")
	}
	repo, admin := projectRecordPostgresWithAdmin(t, 3)
	window := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		org    tracker.OrganizationID
		hosted bool
	}{{"org_local", false}, {"org_hosted", true}}
	ages := []time.Duration{25 * time.Hour, 24 * time.Hour, 23 * time.Hour}
	services := make(map[tracker.OrganizationID]*Service)
	for _, test := range tests {
		d := &database{billingRepository: repo, hostedOrganization: test.org}
		s := &Service{database: d}
		if test.hosted {
			s.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(test.org)}
			s.requestMetrics = newTenantTestMetrics(repo, test.org)
		} else {
			s.localOrganization = string(test.org)
		}
		services[test.org] = s
		if err := repo.WriteTenant(t.Context(), test.org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: test.org, Name: string(test.org), CreatedAt: window.Add(-48 * time.Hour)}); err != nil {
				return err
			}
			for i, age := range ages {
				if err := d.plansService().ChargeScheduledClassification(t.Context(), w, test.org, "prj_example", fmt.Sprintf("classification_%d", i), window.Add(-age)); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	pruned := make(map[tracker.OrganizationID]bool)
	for _, test := range tests {
		t.Run(string(test.org), func(t *testing.T) {
			if err := services[test.org].expireRequestMetrics(t.Context(), window.Add(30*time.Minute)); err != nil {
				t.Fatal(err)
			}
			pruned[test.org] = true
			for _, tenant := range tests {
				for _, age := range ages {
					want := 2
					if pruned[tenant.org] && age >= 24*time.Hour {
						want = 0
					}
					var receipts, windows int
					err := admin.QueryRowContext(t.Context(), `SELECT
						(SELECT count(*) FROM hosted_usage_receipts WHERE organization_id=$1 AND window_start=$2),
						(SELECT count(*) FROM hosted_usage_windows WHERE organization_id=$1 AND window_start=$2)`,
						tenant.org, window.Add(-age)).Scan(&receipts, &windows)
					if err != nil || receipts != want || windows != want {
						t.Errorf("tenant %s age %s: receipts=%d windows=%d, want %d of each, err=%v", tenant.org, age, receipts, windows, want, err)
					}
				}
			}
		})
	}
}
