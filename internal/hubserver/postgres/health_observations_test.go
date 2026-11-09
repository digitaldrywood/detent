package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
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
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				rows, err := r.(healthstore.ObservationReader).HealthUsage(t.Context(), project, now.Add(-time.Minute), now, 10)
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].Item != fmt.Sprintf("wi_%d", id) || rows[0].Cost != float64(100+index) || rows[0].BaselineCost != 0 || rows[0].BaselineKnown != (id == 1) {
					t.Fatalf("organization=%s project=%s usage=%+v", org, project, rows)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
