package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestSummaryUsageRestrictsHistoryBeforePopulationLimit(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
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
