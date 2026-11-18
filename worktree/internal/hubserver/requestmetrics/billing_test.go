package requestmetrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type uncertainUsageRepository struct {
	tenantstore.Repository
	uncertain bool
	failure   error
}

func (r *uncertainUsageRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, pool corestore.Workload, write func(tenantstore.Writer) error) error {
	applied := false
	if err := r.Repository.WriteTenant(ctx, org, pool, func(w tenantstore.Writer) error { return write(uncertainUsageWriter{Writer: w, applied: &applied}) }); err != nil {
		return err
	}
	if r.uncertain && applied {
		r.uncertain = false
		return r.failure
	}
	return nil
}

type uncertainUsageWriter struct {
	tenantstore.Writer
	applied *bool
}

func (w uncertainUsageWriter) ApplyBillingUsage(ctx context.Context, receipt billingstore.UsageReceipt, limit int64) (billingstore.UsageResult, error) {
	*w.applied = true
	return w.Writer.ApplyBillingUsage(ctx, receipt, limit)
}

func TestHostedRequestBillingFlushWithoutSQLite(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("commit response lost")
			wrapper := &uncertainUsageRepository{Repository: repository, uncertain: true, failure: failure}
			owner := &BillingOwner{Repository: wrapper, Organization: org, Plans: func() *BillingPlan { return &BillingPlan{WindowSeconds: 60, RetentionWindows: 10} }}
			key := Key{Minute: now.Unix(), Route: "GET /example", Kind: "anonymous", Status: 2}
			usage := Latency{HTTP: Usage{Requests: 1, Duration: 3, ResponseBytes: 5, RequestBytes: 7, KnownRequests: 1}}
			usage.Observe(time.Microsecond)
			m := &Service{Repository: repository, Organization: org, usage: owner, pending: map[Key]Latency{key: usage}}
			if err := m.Flush(t.Context(), now); !errors.Is(err, failure) {
				t.Fatalf("uncertain flush=%v", err)
			}
			if m.BillingPending == nil {
				t.Fatal("uncertain receipt discarded")
			}
			uncertainBatch := m.BillingPending
			m.pending[key] = usage
			if err := m.Flush(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			if err := m.Flush(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			if m.BillingPending != nil {
				t.Fatal("committed batch retained")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for metric, want := range map[string]int64{"http_requests": 2, "http_duration_microseconds": 6, "http_response_bytes": 10, "http_request_bytes": 14, "http_request_bytes_known": 2} {
					got, err := r.BillingUsage(t.Context(), now, metric)
					if err != nil || got != want {
						t.Fatalf("%s=%d want=%d error=%v", metric, got, want, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := m.Flush(t.Context(), now.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			m.BillingPending = uncertainBatch
			if err := m.FlushBilling(t.Context(), now.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BillingUsages(t.Context(), now, 10)
				if err != nil {
					return err
				}
				if len(rows) != 0 {
					t.Fatalf("expired retry resurrected usage: %+v", rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

		})
	}
}
