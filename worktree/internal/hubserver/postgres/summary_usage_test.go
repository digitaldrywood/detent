package postgres_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSummaryUsageRestrictsHistoryBeforePopulationLimit(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		lease := claimLease(t, repository, org, leaseClaim(1))
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_summary", Fence: leaseFence(lease), RunID: "run_summary", Sequence: 1, Status: "running", StartedAt: at.AddDate(-1, 0, 0), PayloadObjectKey: "example/attempt"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,'prj_1','attempt_summary',$2::timestamptz - interval '1 year' + n*interval '1 second','example','model',1,0,1,1,'usd',$2 FROM generate_series(1,100001) n`, org, at); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,'prj_1','attempt_summary',$2,'example','model',3,0,2,7,'usd',$2),($1,'prj_1','attempt_summary',$2::timestamptz+interval '1 hour','example','model',9,0,9,99,'usd',$2)`, org, at); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner, ok := r.(reportstore.SummaryUsageReader)
			if !ok {
				return fmt.Errorf("typed summary usage owner missing")
			}
			for _, tc := range []struct {
				project   tracker.ProjectID
				principal string
				want      int
			}{{"prj_1", "", 1}, {"prj_2", "", 0}, {"prj_1", "other", 0}} {
				rows, err := owner.ReadSummaryUsage(t.Context(), tc.project, tc.principal, at, at.Add(time.Hour), nil)
				if err != nil {
					return err
				}
				if len(rows) != tc.want {
					t.Fatalf("rows=%d want=%d", len(rows), tc.want)
				}
				if len(rows) > 0 && (rows[0].Cost != 7 || rows[0].Input != 3 || rows[0].Output != 2) {
					t.Fatalf("usage=%+v", rows[0])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
