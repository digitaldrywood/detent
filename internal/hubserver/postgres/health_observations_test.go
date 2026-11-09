package postgres_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthUsageDistinguishesMissingAndZeroBaseline(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		for id := int64(1); id <= 2; id++ {
			lease := claimLease(t, repository, org, leaseClaim(id))
			attempt := attemptEvent(lease)
			if err := repository.WriteLeases(t.Context(), org, corestore.Background, func(w leasestore.Writer) error { _, err := w.RecordAttempt(t.Context(), attempt); return err }); err != nil {
				t.Fatal(err)
			}
			project := tracker.ProjectID(fmt.Sprintf("prj_%d", id))
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'example','example',0,0,0,$5,'USD',$4)`, org, project, attempt.ID, now, float64(100+index)); err != nil {
				t.Fatal(err)
			}
			if id == 1 {
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'example','example',0,0,0,0,'USD',$4)`, org, project, attempt.ID, now.Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'example','example',0,0,0,1000000,'EUR',$4)`, org, project, attempt.ID, now.Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				page, err := r.(healthstore.ObservationReader).HealthUsage(t.Context(), project, now.Add(-time.Minute), now, 10, 10000)
				rows := page.Values
				if err != nil {
					return err
				}
				if !page.Complete || len(rows) != 1 || rows[0].Item != fmt.Sprintf("wi_%d", id) || rows[0].Cost != float64(100+index) || rows[0].BaselineCost != 0 || rows[0].BaselineKnown != (id == 1) {
					t.Fatalf("organization=%s project=%s usage=%+v", org, project, rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			current := billingCostObservation(t, org, project, fmt.Sprintf("health-current-%d", id), attempt.ID, "runner_api", now.Add(-3*time.Hour), now.Add(-2*time.Hour), 2000000)
			current.Observation.AttemptID = attempt.ID
			current.Observation.WorkItemID = fmt.Sprintf("wi_%d", id)
			raw, err := json.Marshal(current.Observation)
			if err != nil {
				t.Fatal(err)
			}
			archive, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			current.Reference = billingstore.AuditReferenceFromArchive(archive)
			if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
				if _, err := w.RecordBillingCosts(t.Context(), []billingstore.CostObservation{current}); err != nil {
					return err
				}
				return w.(reportstore.UsageWriter).RefreshReportUsage(t.Context(), []billingstore.CostObservation{current})
			}); err != nil {
				t.Fatal(err)
			}
			remaining := 9998
			if id == 1 {
				remaining--
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,$3,$4::timestamptz-n*interval '1 millisecond','example','example',0,0,0,0,'USD',$4 FROM generate_series(1,$5::integer) n`, org, project, attempt.ID, now.Add(-24*time.Hour), remaining); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				page, err := r.(healthstore.ObservationReader).HealthUsage(t.Context(), project, now.Add(-time.Minute), now, 10, 10000)
				rows := page.Values
				if err != nil || !page.Complete || page.ReadRows != 10000 || len(rows) != 1 || rows[0].Cost != 2 || rows[0].BaselineCost != 2 || !rows[0].BaselineKnown {
					t.Fatalf("organization=%s project=%s usage at source boundary=%+v err=%v", org, project, rows, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'example','example',0,0,0,0,'USD',$4)`, org, project, attempt.ID, now.Add(-48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				page, err := r.(healthstore.ObservationReader).HealthUsage(t.Context(), project, now.Add(-time.Minute), now, 10, 10000)
				if err != nil || page.Complete || page.ReadRows < 10001 || len(page.Values) != 0 {
					t.Fatalf("organization=%s project=%s source overflow page=%+v err=%v", org, project, page, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
