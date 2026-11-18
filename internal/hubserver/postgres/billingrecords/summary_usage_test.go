package billingrecords_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/reportreads"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestSummaryUsageRestrictsHistoryBeforePopulationLimit(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "attempt_summary", Fence: pgstoretest.LeaseFence(lease), RunID: "run_summary", Sequence: 1, Status: "running", StartedAt: at.AddDate(-1, 0, 0), PayloadObjectKey: "example/attempt"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,'prj_1','attempt_summary',$2::timestamptz - interval '1 year' + n*interval '1 second','example','model',1,0,1,1,'usd',$2 FROM generate_series(1,99998) n`, org, at); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,'prj_1','attempt_summary',$2,'example','model',3,0,2,7,'usd',$2),($1,'prj_1','attempt_summary',$2::timestamptz+interval '1 hour','example','model',9,0,9,99,'usd',$2)`, org, at); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/exact lifetime import allowance", func(t *testing.T) {
			err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				query := reportstore.CompletionCostQuery{Items: []reportstore.CompletionCostItem{{ProjectID: "prj_1", WorkItemID: "wi_1"}}, CurrentTo: at.Add(time.Hour), PriorTo: at}
				rows, err := reader.(reportstore.CompletionCostReader).ReadCompletionCosts(t.Context(), query)
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].Cost != 100005 || rows[0].PriorCost != 99998 || rows[0].Complete {
					t.Fatalf("lifetime costs at 100000 rows = %+v", rows)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,'prj_2','attempt_summary',$2,'example','model',0,0,0,1,'eur',$2)`, org, at.Add(3*time.Hour)); err != nil {
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
			completion := r.(reportstore.CompletionCostReader)
			current := reportstore.CompletionCostQuery{Items: []reportstore.CompletionCostItem{{ProjectID: "prj_2", WorkItemID: "wi_2"}}, CurrentTo: at.Add(time.Hour), PriorTo: at}
			rows, err := completion.ReadCompletionCosts(t.Context(), current)
			if err != nil {
				return err
			}
			if len(rows) != 1 || rows[0].WorkItemID != "wi_2" || !rows[0].Complete || rows[0].Cost != 0 || rows[0].PriorCost != 0 {
				t.Fatalf("unrelated historical population affected empty completion costs: %+v", rows)
			}
			current.Items = append(current.Items, reportstore.CompletionCostItem{ProjectID: "prj_1", WorkItemID: "wi_1"})
			if _, err := completion.ReadCompletionCosts(t.Context(), current); !errors.Is(err, reportstore.ErrPopulationExceeded) {
				t.Fatalf("completion history overflow=%v want population exceeded", err)
			}
			current.Principal = "foreign_actor"
			rows, err = completion.ReadCompletionCosts(t.Context(), current)
			if err != nil {
				return err
			}
			if len(rows) != 0 {
				t.Fatalf("inaccessible historical population returned completion costs: %+v", rows)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(string(org)+"/batch empty attempts at the exact row limit", func(t *testing.T) {
			counter := &pgstoretest.ReportCountingTracer{}
			traced := pgstoretest.TracedFeaturePool(t, pool, counter)
			pools, err := pgtransport.Adopt(traced, traced)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := pools.Begin(t.Context(), pgtransport.Scope{Organization: org}, pgtransport.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			attempts := []reportstore.AttemptFact{{AttemptID: "attempt_summary", ProjectID: "prj_1"}}
			for i := range 64 {
				attempts = append(attempts, reportstore.AttemptFact{AttemptID: fmt.Sprintf("empty-%d", i), ProjectID: "prj_1"})
			}
			counter.Count.Store(0)
			rows, err := reportreads.UsageRows(t.Context(), tx, org, at, at.Add(time.Hour), 1, attempts, false)
			if err != nil || len(rows) != 1 || rows[0].Cost != 7 {
				t.Fatalf("exact-limit rows=%v error=%v", rows, err)
			}
			if statements := counter.Count.Load(); statements > 3 {
				t.Fatalf("exact-limit read with empty attempts used %d statements", statements)
			}
		})
		t.Run(string(org)+"/tied usage keys count before currency filtering", func(t *testing.T) {
			period := at.Add(2 * time.Hour)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at)
SELECT $1,'prj_1','attempt_summary',$2,'example','model-'||n,n,0,0,n,CASE WHEN n=4 THEN 'eur' ELSE 'usd' END,$2 FROM generate_series(1,6) n`, org, period); err != nil {
				t.Fatal(err)
			}
			err := repository.ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				usage := reader.(reportstore.UsageReader)
				query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{"prj_1"}, From: period, To: period.Add(time.Second), Limit: 6}
				rows, err := usage.ReadReportUsage(t.Context(), query)
				if err != nil {
					return err
				}
				var inputs []int64
				for _, row := range rows {
					inputs = append(inputs, row.Input)
				}
				if !slices.Equal(inputs, []int64{1, 2, 3, 5, 6}) {
					t.Fatalf("windowed inputs = %v", inputs)
				}
				query.Limit = 5
				if _, err := usage.ReadReportUsage(t.Context(), query); !errors.Is(err, reportstore.ErrPopulationExceeded) {
					t.Fatalf("six raw rows at limit five = %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
		t.Run(string(org)+"/zero billing authority", func(t *testing.T) {
			objects := pgstoretest.NewHistoryObjects()
			content := eventstore.NewContent(objects)
			lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(2))
			fence := pgstoretest.LeaseFence(lease)
			run := tracker.NativeRunData{AttemptID: "attempt_summary_zero", RunID: "run_summary_zero", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "Example setup failure"}}
			ref, err := content.PreparePayload(t.Context(), org, pgstoretest.HistoryJSON(t, run))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, RunID: run.RunID, Fence: fence, Sequence: 1, Status: "failed", StartedAt: at, PayloadObjectKey: ref.Key, PayloadReference: ref}); err != nil {
					return err
				}
				_, err := w.Release(t.Context(), fence, "completed")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var tokenRecorded, canonicalCost, importedCost bool
			if err := admin.QueryRowContext(t.Context(), `SELECT a.runner_tokens_recorded,
EXISTS(SELECT 1 FROM cost_sources c WHERE c.organization_id=a.organization_id AND c.resource_id=a.id AND c.bucket='runner_api'),
EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id)
FROM native_attempts a WHERE a.organization_id=$1 AND a.id=$2`, org, run.AttemptID).Scan(&tokenRecorded, &canonicalCost, &importedCost); err != nil {
				t.Fatal(err)
			}
			if !tokenRecorded || canonicalCost || importedCost {
				t.Fatalf("zero-cost premise: token-recorded=%t canonical=%t imported=%t", tokenRecorded, canonicalCost, importedCost)
			}
			for _, check := range []struct {
				name  string
				known bool
			}{
				{"token-recorded cost absence stays unknown", false},
				{"explicit complete USD zero is known", true},
			} {
				t.Run(check.name, func(t *testing.T) {
					if check.known {
						observation := usagecost.Observation{Provider: "example", ProviderAccount: "runner", SourceID: "attempt_summary_zero:model", ResourceID: run.AttemptID, AttemptID: run.AttemptID, WorkItemID: "wi_2", Model: "example-model", Bucket: "runner_api", Metric: "example-model", Unit: "token", Currency: "USD", Revision: 1, From: at.Add(-time.Minute), To: at, AmountMicros: new(int64(0)), Coverage: "complete"}
						ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, observation))
						if err != nil {
							t.Fatal(err)
						}
						record := billingstore.CostObservation{OrganizationID: org, ProjectID: "prj_2", Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: at}
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							if _, err := w.RecordBillingCosts(t.Context(), []billingstore.CostObservation{record}); err != nil {
								return err
							}
							return w.(reportstore.UsageWriter).RefreshReportUsage(t.Context(), []billingstore.CostObservation{record})
						}); err != nil {
							t.Fatal(err)
						}
					}
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						rows, err := r.(reportstore.CompletionCostReader).ReadCompletionCosts(t.Context(), reportstore.CompletionCostQuery{Items: []reportstore.CompletionCostItem{{ProjectID: "prj_2", WorkItemID: "wi_2"}}, CurrentTo: at.Add(time.Hour), PriorTo: at})
						if err != nil {
							return err
						}
						if len(rows) != 1 || rows[0].Complete != check.known || rows[0].Cost != 0 || rows[0].PriorCost != 0 {
							t.Fatalf("completion zero billing rows=%+v want known%t", rows, check.known)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})

	}
}
