package billingrecords_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/activitystore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresReportsReadTypedWorkItemFacts(t *testing.T) {
	t.Run("analytics landing excludes corrupt request pointers", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		for _, org := range []tracker.OrganizationID{"org_landing_first", "org_landing_second"} {
			pgstoretest.SeedRepository(t, repository, org)
			first := pgstoretest.SeedHistory(t, repository, org, "defensive_first")
			second := pgstoretest.SeedHistory(t, repository, org, "defensive_second")
			for _, fixture := range []pgstoretest.HistoryFixture{first, second} {
				value, err := fixture.Content.Change(t.Context(), fixture.Change)
				if err != nil {
					t.Fatal(err)
				}
				value.Revision++
				value.CurrentVersion = fixture.Version.ID
				value.Landed = &tracker.ChangeLanding{VersionID: fixture.Version.ID, HeadSHA: fixture.Version.HeadSHA, MergeSHA: strings.Repeat("a", 40), BaseRef: "develop", LandedAt: pgstoretest.HistoryAt(), Actor: pgstoretest.HistoryActor()}
				record, err := fixture.Content.PrepareChange(t.Context(), org, pgstoretest.HistoryJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				record.Revision = fixture.Change.Revision
				if err := repository.WriteRecords(t.Context(), org, corestore.Serving, func(w eventstore.Writer) error { _, err := w.UpdateChange(t.Context(), record); return err }); err != nil {
					t.Fatal(err)
				}
			}
			read := func(t *testing.T) eventstore.AnalyticsLandingPage {
				t.Helper()
				var page eventstore.AnalyticsLandingPage
				if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
					var err error
					page, err = r.(eventstore.AnalyticsLandingReader).ReadAnalyticsLandingPage(t.Context(), eventstore.AnalyticsLandingQuery{ProjectID: "prj_first", From: pgstoretest.HistoryAt().Add(-time.Hour), To: pgstoretest.HistoryAt().Add(time.Hour), Limit: 10})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			if page := read(t); len(page.Items) != 2 {
				t.Fatalf("valid landing population=%+v", page)
			}
			for _, test := range []struct{ name, column, value string }{
				{"mismatched request head", "landed_head_sha", "different-head"},
				{"empty merge identity", "landed_merge_sha", ""},
				{"absent landed version", "landed_version_id", "missing-version"},
			} {
				t.Run(string(org)+"/"+test.name, func(t *testing.T) {
					if _, err := admin.ExecContext(t.Context(), "UPDATE change_requests SET "+test.column+"=$3 WHERE organization_id=$1 AND id=$2", org, first.Change.ID, test.value); err != nil {
						t.Fatal(err)
					}
					page := read(t)
					if len(page.Items) != 1 || page.Items[0].ChangeID != second.Change.ID || page.Items[0].VersionID != second.Version.ID {
						t.Fatalf("invalid landing leaked or valid second lost: page=%+v", page)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET landed_head_sha=$3,landed_merge_sha=$4,landed_version_id=$5 WHERE organization_id=$1 AND id=$2`, org, first.Change.ID, first.Version.HeadSHA, strings.Repeat("a", 40), first.Version.ID); err != nil {
						t.Fatal(err)
					}
					if page := read(t); len(page.Items) != 2 {
						t.Fatalf("restored landing population=%+v", page)
					}
				})
			}
		}
	})
	t.Run("analytics residence seeks selected project history before residuals", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		base := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
		for _, org := range []tracker.OrganizationID{"org_residence_first", "org_residence_second"} {
			pgstoretest.SeedRepository(t, repository, org)
			pgstoretest.SeedHistory(t, repository, org, "residence")
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO project_workflow_states(organization_id,project_id,name,terminal,dispatchable,board_rank)
 SELECT $1,p.id,s.name,s.terminal,s.dispatchable,s.rank FROM (VALUES('prj_first'),('prj_second'))p(id)
 CROSS JOIN (VALUES('Todo',false,true,0),('In Progress',false,true,1),('Done',true,false,2))s(name,terminal,dispatchable,rank)`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT $1,'prj_first','wi_first_1','noise-'||n,n+1,'run.observed',1,'','',$2::timestamptz,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(1,10000)n
 UNION ALL SELECT $1,'prj_first','wi_first_1','residence-'||n,n+10001,'workflow.transitioned',1,'Todo','In Progress',$2::timestamptz,'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(1,1001)n`, org, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,created_at,native_updated_at) SELECT $1,'prj_second','noise-'||n,n+100,'Other project','Todo',$2::timestamptz,$2::timestamptz FROM generate_series(1,10000)n`, org, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `ANALYZE collaboration_events`); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `ANALYZE issues`); err != nil {
				t.Fatal(err)
			}
			query := eventstore.AnalyticsResidenceQuery{ProjectID: "prj_first", From: base.Add(-time.Hour), To: base.Add(time.Hour), Limit: 1000}
			read := func(query eventstore.AnalyticsResidenceQuery) eventstore.AnalyticsResidencePage {
				t.Helper()
				var page eventstore.AnalyticsResidencePage
				if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(r eventstore.Reader) error {
					var err error
					page, err = r.(eventstore.AnalyticsResidenceReader).ReadAnalyticsResidencePage(t.Context(), query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			head := read(query)

			if len(head.Issues) != 2 || head.IssuesMore || len(head.Events) != 1000 || !head.EventsMore || head.Events[0].Sequence != 10002 || head.EventsAfterSequence != 11001 {
				t.Fatalf("residence head issues=%d events=%d cursor=%d more=%v", len(head.Issues), len(head.Events), head.EventsAfterSequence, head.EventsMore)
			}
			for _, event := range head.Events {
				if event.OrganizationID != org || event.ProjectID != "prj_first" || event.WorkItemID != "wi_first_1" {
					t.Fatalf("residence event crossed scope: %+v", event)
				}
			}
			query.EventsAfterItem, query.EventsAfterSequence = head.EventsAfterItem, head.EventsAfterSequence
			tail := read(query)

			if len(tail.Events) != 1 || tail.EventsMore || tail.Events[0].Sequence != 11002 {
				t.Fatalf("residence tail=%+v", tail)
			}
			query.Principal = "another-author"
			hidden := read(query)
			if len(hidden.Issues) != 0 || len(hidden.Events) != 0 {
				t.Fatalf("residence exposed another author's rows: issues=%d events=%d", len(hidden.Issues), len(hidden.Events))
			}
		}
	})
	t.Run("analytics usage seeks both sources before visibility and override residuals", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		for _, org := range []tracker.OrganizationID{"org_usage_first", "org_usage_second"} {
			pgstoretest.SeedLeaseFamily(t, repository, org)
			for _, source := range []struct {
				issue   int64
				attempt string
				project string
			}{{1, "usage_import", "prj_1"}, {2, "usage_other", "prj_2"}} {
				lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(source.issue))
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: source.attempt, Fence: pgstoretest.LeaseFence(lease), RunID: source.attempt + "-run", Sequence: 1, Status: "succeeded", StartedAt: base, PayloadObjectKey: "fixture/usage"}); err != nil {
						return err
					}
					_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "completed")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				count := 1001
				if source.project == "prj_2" {
					count = 10000
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,$3,$4,'example','model-'||lpad(n::text,4,'0'),3,1,2,7,'USD',$4 FROM generate_series(0,$5::integer-1)n`, org, source.project, source.attempt, base, count); err != nil {
					t.Fatal(err)
				}
			}
			read := func(query reportstore.AnalyticsUsageQuery) reportstore.AnalyticsUsagePage {
				t.Helper()
				var page reportstore.AnalyticsUsagePage
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(t.Context(), query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE attempt_usage"); err != nil {
				t.Fatal(err)
			}
			query := reportstore.AnalyticsUsageQuery{ProjectID: "prj_1", From: base, To: base.Add(time.Hour), Limit: 1000}
			head := read(query)

			if len(head.Items) != 1000 || head.Examined != 1001 || !head.Partial || head.Next.Model != "model-0999" || head.Next.Source != 1 || head.Items[0].Model != "model-0000" || head.Items[0].Cost != 7 || head.Items[0].Input != 3 || head.Items[0].Output != 2 {
				t.Fatalf("usage head=%+v rows=%d", head.Next, len(head.Items))
			}
			query.After = head.Next
			tail := read(query)

			if len(tail.Items) != 1 || tail.Items[0].Model != "model-1000" || tail.Examined != 1 || !tail.Partial || !tail.Next.Period.IsZero() {
				t.Fatalf("usage tail=%+v", tail)
			}
			denseQuery := reportstore.AnalyticsUsageQuery{ProjectID: "prj_2", From: base, To: base.Add(time.Hour), Limit: 1000}
			denseHead := read(denseQuery)

			if len(denseHead.Items) != 1000 || denseHead.Examined != 1001 || denseHead.Next.Model != "model-0999" || denseHead.Items[0].AttemptID != "usage_other" || !denseHead.Partial {
				t.Fatalf("dense usage source head rows=%d examined=%d next=%+v", len(denseHead.Items), denseHead.Examined, denseHead.Next)
			}
			denseQuery.After = denseHead.Next
			denseNext := read(denseQuery)

			if len(denseNext.Items) != 1000 || denseNext.Examined != 1001 || denseNext.Items[0].Model != "model-1000" || denseNext.Next.Model != "model-1999" || !denseNext.Partial {
				t.Fatalf("dense usage continuation rows=%d examined=%d next=%+v", len(denseNext.Items), denseNext.Examined, denseNext.Next)
			}
			query.After = reportstore.AnalyticsUsageCursor{}
			query.Principal = "unrelated-principal"
			hidden := read(query)
			if len(hidden.Items) != 0 || hidden.Examined != 1001 || hidden.Next.Model != "model-0999" || !hidden.Partial {
				t.Fatalf("usage visibility residual=%+v", hidden)
			}
			query.Principal = ""
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO cost_sources(organization_id,source_key,project_id,provider,provider_account,resource_id,bucket,metric,period_start,period_end,revision,attribution_hash,payload_hash,amount_micros,currency,voided) SELECT $1,'current-'||n,'prj_1','example','example','usage_import','runner_api','cost','2026-11-11T12:00:00Z','2026-11-11T13:00:00Z',1,'fixture','fixture',1000000,'USD',false FROM generate_series(0,1000)n`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete) SELECT $1,'current-'||n,1,'prj_1','usage_import',$2,'example','model-'||lpad(n::text,4,'0'),4,1,3,9,true FROM generate_series(0,1000)n`, org, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO cost_sources(organization_id,source_key,project_id,provider,provider_account,resource_id,bucket,metric,period_start,period_end,revision,attribution_hash,payload_hash,amount_micros,currency,voided) SELECT $1,'other-current-'||n,'prj_2','example','example','usage_other','runner_api','cost','2026-11-11T12:00:00Z','2026-11-11T13:00:00Z',1,'fixture','fixture',1000000,'USD',false FROM generate_series(0,9999)n`, org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete) SELECT $1,'other-current-'||n,1,'prj_2','usage_other',$2,'example','model-'||lpad(n::text,4,'0'),4,1,3,9,true FROM generate_series(0,9999)n`, org, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE report_usage_current"); err != nil {
				t.Fatal(err)
			}
			rawHead := read(query)

			if len(rawHead.Items) != 500 || rawHead.Examined != 1001 || !rawHead.Partial || rawHead.Next.Source != 1 || rawHead.Next.Model != "model-0499" {
				t.Fatalf("current override first raw page rows=%d examined=%d next=%+v", len(rawHead.Items), rawHead.Examined, rawHead.Next)
			}
			query.After = rawHead.Next
			rawNext := read(query)

			if len(rawNext.Items) != 500 || rawNext.Examined != 1001 || !rawNext.Partial || rawNext.Next.Model != "model-0999" || rawNext.Next.Source != 1 {
				t.Fatalf("current override second raw page rows=%d examined=%d next=%+v", len(rawNext.Items), rawNext.Examined, rawNext.Next)
			}
			query.After = rawNext.Next
			rawTail := read(query)

			if len(rawTail.Items) != 1 || rawTail.Items[0].Model != "model-1000" || rawTail.Items[0].Cost != 9 || rawTail.Items[0].Input != 4 || rawTail.Items[0].Output != 3 || rawTail.Examined != 2 || !rawTail.Partial || !rawTail.Next.Period.IsZero() {
				t.Fatalf("current override tail=%+v", rawTail)
			}
			emptyClaim := pgstoretest.LeaseClaim(1)
			emptyClaim.LeaseID = "empty-usage-lease"
			emptyClaim.SessionID = "empty-usage-session"
			emptyLease := pgstoretest.ClaimLease(t, repository, org, emptyClaim)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "usage_empty_dimensions", Fence: pgstoretest.LeaseFence(emptyLease), RunID: "usage_empty_dimensions_run", Sequence: 1, Status: "succeeded", StartedAt: base, PayloadObjectKey: "fixture/usage"}); err != nil {
					return err
				}
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(emptyLease), "completed")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,'prj_1','usage_empty_dimensions',$2,'','',1,0,1,11,'USD',$2),($1,'prj_1','usage_empty_dimensions',$2::timestamptz+interval '1 microsecond','example','tail',2,0,2,13,'USD',$2)`, org, base.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			emptyQuery := reportstore.AnalyticsUsageQuery{ProjectID: "prj_1", From: base.Add(2 * time.Hour), To: base.Add(3 * time.Hour), Limit: 1}
			emptyHead := read(emptyQuery)
			if len(emptyHead.Items) != 1 || emptyHead.Items[0].Provider != "" || emptyHead.Items[0].Model != "" || emptyHead.Items[0].Cost != 11 || emptyHead.Next.AttemptID != "usage_empty_dimensions" || emptyHead.Next.Provider != "" || emptyHead.Next.Model != "" || emptyHead.Examined != 2 || !emptyHead.Partial {
				t.Fatalf("empty-dimension usage head=%+v", emptyHead)
			}
			emptyQuery.After = emptyHead.Next
			emptyTail := read(emptyQuery)
			if len(emptyTail.Items) != 1 || emptyTail.Items[0].Model != "tail" || emptyTail.Items[0].Cost != 13 || emptyTail.Examined != 1 || !emptyTail.Partial || !emptyTail.Next.Period.IsZero() {
				t.Fatalf("empty-dimension continuation=%+v", emptyTail)
			}

		}
	})
	t.Run("analytics attempts retain start keyset and bounded residuals", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		base := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
		for _, org := range []tracker.OrganizationID{"org_analytics_first", "org_analytics_second"} {
			pgstoretest.SeedLeaseFamily(t, r, org)
			for _, source := range []struct {
				project tracker.ProjectID
				issue   int64
				prefix  string
				count   int
			}{{"prj_1", 1, "analytics", 1001}, {"prj_2", 2, "other", 10000}} {
				statements := []string{
					`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT $1,$3||'-run-'||n,$2 FROM generate_series(0,$4::integer-1)n`,
					`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,$3||'-lease-'||n,$2,'machine_1',$3||'-session-'||n,1,$5,$5,$5::timestamptz+interval '1 hour',$5::timestamptz+interval '1 second' FROM generate_series(0,$4::integer-1)n`,
					`INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at) SELECT $1,$6,$3||'-'||lpad(n::text,4,'0'),$2,l.lease_id,l.fencing_token,$3||'-run-'||n,1,'succeeded',1,1,'fixture/analytics',$5,n%3,$5::timestamptz+interval '1 minute' FROM generate_series(0,$4::integer-1)n JOIN leases l ON l.organization_id=$1 AND l.lease_id=$3||'-lease-'||n`,
				}
				for index, statement := range statements {
					args := []any{org, source.issue, source.prefix, source.count, base, source.project}
					switch index {
					case 0:
						args = args[:4]
					case 1:
						args = args[:5]
					}
					if _, err := admin.ExecContext(t.Context(), statement, args...); err != nil {
						t.Fatal(err)
					}
				}
			}
			var head reportstore.AnalyticsAttemptPage
			query := reportstore.AnalyticsAttemptQuery{ProjectID: "prj_1", From: base.Add(-time.Hour), To: base.Add(time.Hour), Limit: 1000}
			read := func(query reportstore.AnalyticsAttemptQuery) reportstore.AnalyticsAttemptPage {
				t.Helper()
				var page reportstore.AnalyticsAttemptPage
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
					owner, ok := reader.(reportstore.AnalyticsAttemptReader)
					if !ok {
						return errors.New("analytics attempt reader unavailable")
					}
					var err error
					page, err = owner.ReadAnalyticsAttemptPage(t.Context(), query)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return page
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE native_attempts"); err != nil {
				t.Fatal(err)
			}
			head = read(query)

			if len(head.Items) != 1000 || head.Examined != 1001 || !head.Partial || head.Next.ID != "analytics-0003" || head.Next.Submicro != 0 || head.Items[0].AttemptID != "analytics-0998" || head.Items[1].AttemptID != "analytics-0995" {
				t.Fatalf("start-keyset head: rows=%d examined=%d partial=%v next=%+v first=%+v", len(head.Items), head.Examined, head.Partial, head.Next, head.Items[:min(2, len(head.Items))])
			}
			query.After = head.Next
			tail := read(query)

			if len(tail.Items) != 1 || tail.Items[0].AttemptID != "analytics-0000" || !tail.Items[0].StartedAt.Equal(base) || tail.Examined != 1 || !tail.Partial || tail.Next.ID != "" {
				t.Fatalf("start-keyset tail: %+v", tail)
			}
			for index, statement := range []string{
				`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT $1,'history-run-'||n,1 FROM generate_series(0,9999)n`,
				`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,'history-lease-'||n,1,'machine_1','history-session-'||n,1,$2,$2,$2::timestamptz+interval '1 hour',$2::timestamptz+interval '1 second' FROM generate_series(0,9999)n`,
				`INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at) SELECT $1,'prj_1','history-'||lpad(n::text,4,'0'),1,l.lease_id,l.fencing_token,'history-run-'||n,1,'succeeded',1,1,'fixture/analytics',$2,n%3,$2::timestamptz+interval '1 minute' FROM generate_series(0,9999)n JOIN leases l ON l.organization_id=$1 AND l.lease_id='history-lease-'||n`,
			} {
				args := []any{org, base.AddDate(-1, 0, 0)}
				if index == 0 {
					args = args[:1]
				}
				if _, err := admin.ExecContext(t.Context(), statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE native_attempts"); err != nil {
				t.Fatal(err)
			}
			query.After = reportstore.AnalyticsAttemptCursor{}
			recent := read(query)

			if len(recent.Items) != 1000 || recent.Examined != 1001 || recent.Items[0].AttemptID != "analytics-0998" || recent.Next.ID != "analytics-0003" || !recent.Partial {
				t.Fatalf("old history displaced recent dashboard attempts: rows=%d examined=%d first=%+v next=%+v", len(recent.Items), recent.Examined, recent.Items[:min(1, len(recent.Items))], recent.Next)
			}
			query.After = reportstore.AnalyticsAttemptCursor{}
			query.Principal = "unrelated-principal"
			hidden := read(query)
			if len(hidden.Items) != 0 || hidden.Examined != 1001 || hidden.Next.ID != "analytics-0003" || !hidden.Partial {
				t.Fatalf("visibility residual exceeded source budget: %+v", hidden)
			}
			query.Principal = ""
			query.RunnerID = "unrelated-runner"
			runner := read(query)
			if len(runner.Items) != 0 || runner.Examined != 1001 || runner.Next.ID != "analytics-0003" || !runner.Partial {
				t.Fatalf("runner residual exceeded source budget: %+v", runner)
			}
			query.RunnerID = ""
			query.From = base.Add(30 * time.Minute)
			stale := read(query)
			if len(stale.Items) != 0 || stale.Examined != 1001 || stale.Next.ID != "analytics-0003" || !stale.Partial {
				t.Fatalf("updated-window residual exceeded source budget: %+v", stale)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET project_id='prj_2' WHERE organization_id=$1 AND id='analytics-0000'`, org); err == nil {
				t.Fatal("attempt accepted mismatched project identity")
			} else {
				var failure *pgconn.PgError
				if !errors.As(err, &failure) || failure.Code != "23503" || failure.ConstraintName != "native_attempts_project_identity" {
					t.Fatalf("project identity failure=%v", err)
				}
			}
		}
	})
	t.Run("issue history truncation preserves complete activity window", func(t *testing.T) {
		for _, kind := range []string{"transitions", "attempts"} {
			t.Run(kind, func(t *testing.T) {
				r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
				objects := pgstoretest.NewHistoryObjects()
				content := eventstore.NewContent(objects)
				now := time.Now().UTC().Truncate(time.Microsecond)
				old := now.Add(-24 * time.Hour)
				for _, org := range []tracker.OrganizationID{"org_history_first", "org_history_second"} {
					pgstoretest.SeedLeaseFamily(t, r, org)
					receipt := func(operation, key string) eventstore.Receipt {
						t.Helper()
						value, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "production-import", Operation: operation, CommandKey: key, RequestHash: fmt.Sprintf("%x", sha256.Sum256([]byte(operation+key))), CreatedAt: now}, []byte(`{}`))
						if err != nil {
							t.Fatal(err)
						}
						return value
					}
					for issue := int64(1); issue <= 2; issue++ {
						project := tracker.ProjectID(fmt.Sprintf("prj_%d", issue))
						item := tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", issue))
						if kind == "transitions" {
							for index := 0; index <= 50; index++ {
								event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("transition_%d_%d", issue, index), OrganizationID: org, ProjectID: project, AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: old.Add(time.Duration(index) * time.Minute), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Todo"}}))
								if err != nil {
									t.Fatal(err)
								}
								if err := r.WriteTenant(t.Context(), org, corestore.Background, func(writer tenantstore.Writer) error { _, err := writer.AppendEvent(t.Context(), event); return err }); err != nil {
									t.Fatal(err)
								}
							}
						} else {
							for index := 0; index <= 50; index++ {
								id := fmt.Sprintf("historical_%d_%d", issue, index)
								end := old.Add(time.Minute)
								lease := leasestore.Lease{OrganizationID: org, LeaseID: id, FencingToken: issue*1000 + int64(index) + 1, IssueID: issue, MachineID: fmt.Sprintf("machine_%d", issue), SessionID: id, WorkItemRevision: 1, AcquiredAt: old, RenewedAt: old, ExpiresAt: old.Add(time.Hour), ReleasedAt: &end}
								if err := r.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: receipt("core-lease", id)}); err != nil {
									t.Fatal(err)
								}
								ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(id), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded"}))
								if err != nil {
									t.Fatal(err)
								}
								attempt := leasestore.Attempt{OrganizationID: org, ID: id, IssueID: issue, LeaseID: id, FencingToken: lease.FencingToken, RunID: id, Sequence: 1, Status: "succeeded", WorkItemRevision: 1, PayloadObjectKey: ref.Key, StartedAt: old, UpdatedAt: end}
								if err := r.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt("core-attempt", id)}); err != nil {
									t.Fatal(err)
								}
							}
						}
						lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(issue))
						id := fmt.Sprintf("current_%d", issue)
						run := tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1}
						ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
						if err != nil {
							t.Fatal(err)
						}
						if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
							_, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: id, Fence: pgstoretest.LeaseFence(lease), RunID: id, Sequence: 1, Status: "running", StartedAt: now.Add(-time.Minute), PayloadObjectKey: ref.Key})
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					observed := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
					for _, project := range []tracker.ProjectID{"prj_1", "prj_2"} {
						if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
							snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Hour), To: observed, Now: observed, Limit: 1})
							if err != nil {
								return err
							}
							if snapshot.Partial || snapshot.DurationsPartial || !snapshot.IssuesPartial || len(snapshot.Durations) != 0 || len(snapshot.Issues) != 1 || !snapshot.Issues[0].Partial {
								t.Fatalf("history truncation contaminated window: window=%t durations=%t issues=%t numeric=%d issue=%+v", snapshot.Partial, snapshot.DurationsPartial, snapshot.IssuesPartial, len(snapshot.Durations), snapshot.Issues)
							}
							running, finished := 0, 0
							for _, attempt := range snapshot.Attempts {
								if attempt.Selected {
									if attempt.Live {
										running++
									} else {
										finished++
									}
								}
							}
							if running != 1 || finished != 0 {
								t.Fatalf("history changed selected window: running=%d finished=%d", running, finished)
							}
							if kind == "transitions" && len(snapshot.Transitions) != 50 {
								t.Fatalf("transition budget changed: %d", len(snapshot.Transitions))
							}
							if kind == "attempts" && len(snapshot.Attempts) != 50 {
								t.Fatalf("attempt history budget changed: %d", len(snapshot.Attempts))
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	})
	t.Run("running overlap retains the ten thousand duration clip", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		const org tracker.OrganizationID = "org_activity_cap"
		pgstoretest.SeedLeaseFamily(t, r, org)
		objects := pgstoretest.NewHistoryObjects()
		content := eventstore.NewContent(objects)
		base := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Minute)
		for _, index := range []int{0, 10000} {
			if err := r.WriteTenant(t.Context(), org, corestore.Background, func(writer tenantstore.Writer) error {
				id := fmt.Sprintf("activity_cap_%05d", index)
				lease, err := writer.Claim(t.Context(), leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "machine_1", SessionID: id, LeaseID: id, TTL: time.Hour})
				if err != nil {
					return err
				}
				status, kind := "succeeded", "run.finished"
				if index == 10000 {
					status, kind = "running", "run.observed"
				}
				run := tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: status, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", base), Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Minute)}}}}
				if status == "running" {
					run.Outcome = ""
				}
				ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
				if err != nil {
					return err
				}
				if _, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: id, Fence: pgstoretest.LeaseFence(lease), RunID: id, Sequence: 1, Status: status, StartedAt: base, PayloadObjectKey: ref.Key}); err != nil {
					return err
				}
				event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: kind, SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: base.Add(time.Minute), Data: tracker.CollaborationData{Run: &run}}))
				if err != nil {
					return err
				}
				if _, err := writer.AppendEvent(t.Context(), event); err != nil {
					return err
				}
				if status != "running" {
					if _, err := writer.Release(t.Context(), pgstoretest.LeaseFence(lease), "completed"); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if index != 0 {
				continue
			}
			rows, err := admin.QueryContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at,release_reason)
 SELECT organization_id,'activity_cap_'||lpad(n::text,5,'0'),issue_id,machine_id,'activity_cap_'||lpad(n::text,5,'0'),work_item_revision,acquired_at,renewed_at,expires_at,released_at,release_reason
 FROM leases CROSS JOIN generate_series(1,9999)n WHERE organization_id=$1 AND lease_id='activity_cap_00000'
 RETURNING lease_id,fencing_token`, org)
			if err != nil {
				t.Fatal(err)
			}
			var ids, runKeys, eventKeys, eventHashes []string
			var eventBytes, eventContentBytes []int64
			for rows.Next() {
				var id string
				var fence int64
				if err := rows.Scan(&id, &fence); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				run := tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(id), FencingToken: tracker.FencingToken(fence), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", base), Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Minute)}}}}
				ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: base.Add(time.Minute), Data: tracker.CollaborationData{Run: &run}}))
				if err != nil {
					rows.Close()
					t.Fatal(err)
				}
				ids = append(ids, id)
				runKeys = append(runKeys, ref.Key)
				eventKeys = append(eventKeys, event.PayloadObjectKey)
				eventHashes = append(eventHashes, event.PayloadSHA256)
				eventBytes = append(eventBytes, event.PayloadBytes)
				eventContentBytes = append(eventContentBytes, event.PayloadContentBytes)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if len(ids) != 9999 {
				t.Fatalf("duration fixture population=%d want=9999", len(ids))
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempt_runs(organization_id,run_id,issue_id)
 SELECT l.organization_id,x.id,l.issue_id FROM unnest($2::text[]) x(id) JOIN leases l ON l.organization_id=$1 AND l.lease_id=x.id`, org, ids); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at)
 SELECT l.organization_id,'prj_1',x.id,l.issue_id,l.lease_id,l.fencing_token,x.id,1,'succeeded',l.work_item_revision,1,x.payload,$4,0,$4::timestamptz+interval '1 minute'
 FROM unnest($2::text[],$3::text[]) x(id,payload) JOIN leases l ON l.organization_id=$1 AND l.lease_id=x.id`, org, ids, runKeys, base); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,project_sequence,type,schema_version,actor_kind,actor_principal_id,actor_envelope_known,actor_via_key,actor_display_name,actor_agent,run_id,attempt_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT $1,'prj_1','wi_1','event_'||x.id,seed.sequence+x.ordinality,seed.project_sequence+x.ordinality,'run.finished',1,seed.actor_kind,seed.actor_principal_id,seed.actor_envelope_known,seed.actor_via_key,seed.actor_display_name,seed.actor_agent,x.id,x.id,$7,x.payload,x.hash,x.bytes,x.content_bytes
 FROM unnest($2::text[],$3::text[],$4::text[],$5::bigint[],$6::bigint[]) WITH ORDINALITY x(id,payload,hash,bytes,content_bytes,ordinality)
 CROSS JOIN collaboration_events seed WHERE seed.organization_id=$1 AND seed.id='event_activity_cap_00000'`, org, ids, eventKeys, eventHashes, eventBytes, eventContentBytes, base.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `WITH issue_event AS (
 UPDATE issues SET event_sequence=(SELECT max(sequence) FROM collaboration_events WHERE organization_id=$1 AND project_id='prj_1' AND work_item_id='wi_1'),last_activity_at=greatest(last_activity_at,$2)
 WHERE organization_id=$1 AND project_id='prj_1' AND native_id='wi_1' RETURNING organization_id,project_id
) INSERT INTO project_event_cursors(organization_id,project_id,sequence)
 SELECT organization_id,project_id,(SELECT max(project_sequence) FROM collaboration_events WHERE organization_id=$1 AND project_id='prj_1') FROM issue_event
 ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=greatest(project_event_cursors.sequence,EXCLUDED.sequence)`, org, base.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO report_event_facts(organization_id,event_id,duration_seconds,run_sequence,run_status,role,model,effort,phase_timing_status)
 SELECT $1,'event_'||x.id,seed.duration_seconds,seed.run_sequence,seed.run_status,seed.role,seed.model,seed.effort,seed.phase_timing_status
 FROM unnest($2::text[]) x(id) CROSS JOIN report_event_facts seed WHERE seed.organization_id=$1 AND seed.event_id='event_activity_cap_00000'`, org, ids); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO report_attempt_facts(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id)
 SELECT $1,x.id,'event_'||x.id,'event_'||x.id,'event_'||x.id FROM unnest($2::text[]) x(id)`, org, ids); err != nil {
				t.Fatal(err)
			}
		}
		observed := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
			snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: base.Add(-time.Minute), To: observed, Now: observed, Limit: 1})
			if err != nil {
				return err
			}
			if len(snapshot.Durations) != 10000 || !snapshot.DurationsPartial {
				t.Fatalf("running overlap erased clip: durations=%d partial=%t", len(snapshot.Durations), snapshot.DurationsPartial)
			}
			for _, fact := range snapshot.Durations {
				if fact.Status != "succeeded" || fact.Role != "code" || fact.Seconds != 60 || fact.Partial {
					t.Fatalf("clipped numeric sample changed: %+v", fact)
				}
			}
			running, finished := 0, 0
			for _, attempt := range snapshot.Attempts {
				if attempt.Selected {
					if attempt.Live {
						running++
					} else {
						finished++
					}
				}
			}
			if running != 1 || finished != 1 {
				t.Fatalf("presentation limit changed: running=%d finished=%d", running, finished)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("activity duration population ignores presentation limit and preserves completeness", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		objects := pgstoretest.NewHistoryObjects()
		content := eventstore.NewContent(objects)
		base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
		for _, org := range []tracker.OrganizationID{"org_activity_first", "org_activity_second"} {
			pgstoretest.SeedLeaseFamily(t, r, org)
			for index := int64(1); index <= 2; index++ {
				lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(index))
				project := tracker.ProjectID(fmt.Sprintf("prj_%d", index))
				run := tracker.NativeRunData{AttemptID: fmt.Sprintf("activity_attempt_%d", index), RunID: fmt.Sprintf("activity_run_%d", index), LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", base), Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Minute)}}, PhasesDropped: int(index - 1)}}
				event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("activity_event_%d", index), OrganizationID: org, ProjectID: project, AggregateID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index)), Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: base.Add(time.Minute), Data: tracker.CollaborationData{Run: &run}}))
				if err != nil {
					t.Fatal(err)
				}
				runRef, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, Fence: pgstoretest.LeaseFence(lease), RunID: run.RunID, Sequence: 1, Status: run.Outcome, StartedAt: base, PayloadObjectKey: runRef.Key}); err != nil {
						return err
					}
					_, err := w.AppendEvent(t.Context(), event)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var duplicate eventstore.Event
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				duplicate, err = reader.LatestRunEvent(t.Context(), "prj_1", "activity_attempt_1")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			duplicateValue, err := content.Event(t.Context(), duplicate)
			if err != nil {
				t.Fatal(err)
			}
			duplicateValue.ID, duplicateValue.AggregateSequence = "activity_event_repeat", 0
			duplicateValue.RecordedAt = base.Add(61 * time.Second)
			duplicate, err = content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, duplicateValue))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
				_, err := writer.AppendEvent(t.Context(), duplicate)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			observed := time.Now().UTC().Truncate(time.Microsecond).Add(time.Second)
			for _, filter := range []activitystore.Filter{
				{Projects: []tracker.ProjectID{"prj_1", "prj_2"}, From: base.Add(-time.Minute), To: observed, Now: observed, Limit: 1},
				{Projects: []tracker.ProjectID{"prj_1"}, From: base.Add(-time.Minute), To: observed, Now: observed, Limit: 1},
				{Projects: []tracker.ProjectID{"prj_1", "prj_2"}, RunnerID: "absent", From: base.Add(-time.Minute), To: observed, Now: observed, Limit: 1},
			} {
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
					snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), filter)
					if err != nil {
						return err
					}
					want := len(filter.Projects)
					if filter.RunnerID != "" {
						want = 0
					}
					if len(snapshot.Durations) != want || snapshot.DurationsPartial {
						t.Fatalf("duration population=%+v clipped=%t filter=%+v", snapshot.Durations, snapshot.DurationsPartial, filter)
					}
					for _, fact := range snapshot.Durations {
						if fact.Seconds != 60 || fact.Role != "code" || fact.Status != "succeeded" || fact.Partial != (fact.ProjectID == "prj_2") {
							t.Fatalf("duration completeness lost: %+v", fact)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$2 WHERE organization_id=$1 AND id='activity_attempt_1'", org, observed.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name     string
				from, to time.Time
				want     int
			}{
				{name: "later update retains overlapping attempt", from: base, to: observed, want: 1},
				{name: "finish is not the lower overlap boundary", from: base.Add(time.Minute), to: observed, want: 1},
				{name: "overlap may finish at upper boundary", from: base, to: base.Add(time.Minute), want: 1},
				{name: "later update moves current overlap population", from: observed, to: observed.Add(2 * time.Hour), want: 1},
				{name: "repeat finish remains one current sample", from: base.Add(61 * time.Second), to: base.Add(62 * time.Second), want: 1},
				{name: "start at upper boundary excluded", from: base.Add(-time.Minute), to: base},
				{name: "update at lower boundary included", from: observed.Add(time.Hour), to: observed.Add(2 * time.Hour), want: 1},
				{name: "update before lower boundary excluded", from: observed.Add(time.Hour + time.Microsecond), to: observed.Add(2 * time.Hour)},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
						snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: test.from, To: test.to, Now: observed.Add(2 * time.Hour), Limit: 1})
						if err != nil {
							return err
						}
						if len(snapshot.Durations) != test.want {
							t.Fatalf("duration overlap window lost: population=%+v from=%s to=%s want=%d", snapshot.Durations, test.from, test.to, test.want)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$2 WHERE organization_id=$1 AND id='activity_attempt_1'", org, base.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: base, To: observed, Now: observed, Limit: 1})
				if err != nil {
					return err
				}
				if len(snapshot.Durations) != 0 || snapshot.DurationsPartial {
					t.Fatalf("older update entered overlap through event time: %+v partial=%t", snapshot.Durations, snapshot.DurationsPartial)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$2 WHERE organization_id=$1 AND id='activity_attempt_1'", org, observed.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE report_attempt_facts SET outcome_event_id=NULL WHERE organization_id=$1 AND attempt_id='activity_attempt_1'", org); err != nil {
				t.Fatal(err)
			}
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: base, To: observed, Now: observed, Limit: 1})
				if err != nil {
					return err
				}
				if len(snapshot.Durations) != 1 || snapshot.Durations[0].Seconds != 60 || snapshot.DurationsPartial || snapshot.Partial {
					t.Fatalf("missing first outcome hid latest overlap projection: %+v duration_partial=%t issue_partial=%t", snapshot.Durations, snapshot.DurationsPartial, snapshot.Partial)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE report_attempt_facts SET outcome_event_id='activity_event_1' WHERE organization_id=$1 AND attempt_id='activity_attempt_1'", org); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "UPDATE report_event_facts SET phase_timing_status='' WHERE organization_id=$1 AND event_id='activity_event_repeat'", org); err != nil {
				t.Fatal(err)
			}
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: base.Add(-time.Minute), To: observed, Now: observed, Limit: 1})
				if err != nil {
					return err
				}
				if len(snapshot.Durations) != 1 || !snapshot.Durations[0].Partial || snapshot.Durations[0].Seconds != 60 || snapshot.Partial {
					t.Fatalf("unknown historical completeness was guessed: %+v", snapshot.Durations)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("activity durations use current overlap projection", func(t *testing.T) {
		admin := pgstoretest.Template.Database(t)
		r, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
		objects := pgstoretest.NewHistoryObjects()
		content := eventstore.NewContent(objects)
		base := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
		for testIndex, test := range []struct {
			name                               string
			writeEvent, clearOutcome, partial  bool
			resume, running, finishAfterResume bool
			wantCount                          int
			wantSeconds                        float64
			wantStatus                         string
			wantFactPartial                    bool
			outcome                            string
			finish                             time.Time
		}{
			{name: "missing first outcome retains latest overlap projection", writeEvent: true, clearOutcome: true, finish: base.Add(time.Minute), wantCount: 1, wantStatus: "interrupted", wantSeconds: 60},
			{name: "future event timestamp does not move current overlap", writeEvent: true, finish: base.Add(3 * time.Hour), wantCount: 1, wantStatus: "interrupted", wantSeconds: 60},
			{name: "missing latest projection remains partial", wantCount: 1, wantStatus: "interrupted", wantFactPartial: true},
			{name: "interrupted finish before legitimate running reconnect", writeEvent: true, resume: true, finish: base.Add(time.Minute)},
			{name: "successful finish with inconsistent running row", writeEvent: true, running: true, outcome: "succeeded", finish: base.Add(time.Minute)},
			{name: "successful finish after reconnect replaces interrupted duration", writeEvent: true, resume: true, finishAfterResume: true, finish: base.Add(time.Minute), wantCount: 1, wantStatus: "succeeded", wantSeconds: 90},
		} {
			t.Run(test.name, func(t *testing.T) {
				for index, org := range []tracker.OrganizationID{"org_activity_gap_first", "org_activity_gap_second"} {
					org = tracker.OrganizationID(fmt.Sprintf("%s_%d", org, testIndex))
					pgstoretest.SeedLeaseFamily(t, r, org)
					lease := pgstoretest.ClaimLease(t, r, org, pgstoretest.LeaseClaim(1))
					run := tracker.NativeRunData{AttemptID: "gap_attempt", RunID: "gap_run", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "interrupted", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "example", "openai", "high", "", base), Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Minute)}}}}
					if test.outcome != "" {
						run.Outcome = test.outcome
					}
					ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
					if err != nil {
						t.Fatal(err)
					}
					if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(writer tenantstore.Writer) error {
						if _, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, Fence: pgstoretest.LeaseFence(lease), RunID: run.RunID, Sequence: 1, Status: run.Outcome, StartedAt: base, PayloadObjectKey: ref.Key}); err != nil {
							return err
						}
						if !test.writeEvent {
							return nil
						}
						event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "gap_event", OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: test.finish, Data: tracker.CollaborationData{Run: &run}}))
						if err != nil {
							return err
						}
						if _, err := writer.AppendEvent(t.Context(), event); err != nil {
							return err
						}
						if !test.resume {
							return nil
						}
						run.Sequence, run.Outcome = 2, ""
						ref, err := objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
						if err != nil {
							return err
						}
						if _, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, Fence: pgstoretest.LeaseFence(lease), RunID: run.RunID, Sequence: 2, Status: "running", StartedAt: base, PayloadObjectKey: ref.Key}); err != nil {
							return err
						}
						observed, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "gap_reconnected", OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.observed", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: base.Add(61 * time.Second), Data: tracker.CollaborationData{Run: &run}}))
						if err != nil {
							return err
						}
						if _, err := writer.AppendEvent(t.Context(), observed); err != nil {
							return err
						}
						if !test.finishAfterResume {
							return nil
						}
						run.Sequence, run.Outcome = 3, "succeeded"
						run.Runtime.Phases = []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(90 * time.Second)}}
						ref, err = objects.StoreDetail(t.Context(), string(org), pgstoretest.HistoryJSON(t, run))
						if err != nil {
							return err
						}
						if _, err := writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, Fence: pgstoretest.LeaseFence(lease), RunID: run.RunID, Sequence: 3, Status: "succeeded", StartedAt: base, PayloadObjectKey: ref.Key}); err != nil {
							return err
						}
						finished, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "gap_finished", OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: base.Add(90 * time.Second), Data: tracker.CollaborationData{Run: &run}}))
						if err != nil {
							return err
						}
						_, err = writer.AppendEvent(t.Context(), finished)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if test.running {
						if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET status='running' WHERE organization_id=$1 AND id='gap_attempt'", org); err != nil {
							t.Fatal(err)
						}
					}
					if test.clearOutcome {
						if _, err := admin.ExecContext(t.Context(), "UPDATE report_attempt_facts SET outcome_event_id=NULL WHERE organization_id=$1 AND attempt_id='gap_attempt'", org); err != nil {
							t.Fatal(err)
						}
					}
					if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
						snapshot, err := reader.(activitystore.Reader).ActivitySnapshot(t.Context(), activitystore.Filter{Projects: []tracker.ProjectID{"prj_1"}, From: base, To: base.Add(2 * time.Hour), Now: base.Add(2 * time.Hour), Limit: 1})
						if err != nil {
							return err
						}
						if len(snapshot.Durations) != test.wantCount || snapshot.DurationsPartial != test.partial {
							t.Fatalf("overlap evidence=%+v duration_partial=%t want=%t tenant=%d", snapshot.Durations, snapshot.DurationsPartial, test.partial, index)
						}
						if test.wantCount != 0 && (snapshot.Durations[0].Status != test.wantStatus || snapshot.Durations[0].Seconds != test.wantSeconds || (!test.wantFactPartial && snapshot.Durations[0].Role != "code") || snapshot.Durations[0].Partial != test.wantFactPartial) {
							t.Fatalf("final duration after reconnect=%+v want status=%s seconds=%f", snapshot.Durations, test.wantStatus, test.wantSeconds)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	})
	t.Run("history pages advance across unrelated events and freeze the upper sequence", func(t *testing.T) {
		r, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
		content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
		for _, org := range []tracker.OrganizationID{"org_history_first", "org_history_second"} {
			pgstoretest.SeedLeaseFamily(t, r, org)
			for index := 1; index <= 66; index++ {
				kind := "issue.created"
				data := tracker.CollaborationData{}
				if index > 64 {
					kind = "workflow.transitioned"
					data.FromState, data.ToState = "Todo", "Done"
				}
				event, err := content.PrepareEvent(t.Context(), org, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("history_page_%d", index), OrganizationID: org, ProjectID: "prj_1", AggregateID: "wi_1", Type: kind, SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: pgstoretest.HistoryAt().Add(time.Duration(index) * time.Second), Data: data}))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.AppendEvent(t.Context(), event)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				owner := reader.(reportstore.HistoryIssuePageReader)
				first, err := owner.ReadHistoryIssueEvents(t.Context(), "prj_1", "wi_1", reportstore.HistoryEventCursor{Through: 65}, 64)
				if err != nil {
					return err
				}
				if first.Examined != 64 || first.After != 64 || first.Exhausted || len(first.Events) != 0 {
					t.Fatalf("unrelated events lost examined progress: %+v", first)
				}
				last, err := owner.ReadHistoryIssueEvents(t.Context(), "prj_1", "wi_1", reportstore.HistoryEventCursor{After: first.After, Through: 65}, 64)
				if err != nil {
					return err
				}
				if last.Examined != 1 || last.After != 65 || !last.Exhausted || len(last.Events) != 1 || last.Events[0].ID != "history_page_65" || last.Events[0].FromState != "Todo" || last.Events[0].ToState != "Done" {
					t.Fatalf("history pass lost state or crossed its epoch head: %+v", last)
				}
				empty, err := owner.ReadHistoryIssueEvents(t.Context(), "prj_1", "wi_1", reportstore.HistoryEventCursor{After: 65, Through: 65}, 64)
				if err != nil {
					return err
				}
				if empty.Examined != 0 || empty.After != 65 || !empty.Exhausted || len(empty.Events) != 0 {
					t.Fatalf("empty exhausted history page: %+v", empty)
				}
				if _, err := owner.ReadHistoryIssueEvents(t.Context(), "prj_1", "foreign", reportstore.HistoryEventCursor{Through: 65}, 64); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("foreign item history accepted: %v", err)
				}
				if _, err := owner.ReadHistoryIssueEvents(t.Context(), "prj_1", "wi_1", reportstore.HistoryEventCursor{After: 66, Through: 65}, 64); !errors.Is(err, corestore.ErrInvalidCursor) {
					t.Fatalf("invalid history cursor accepted: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	const processEvidenceVersion = 20261109184650
	admin, fixture := pgstoretest.MigrationDatabaseAtVersion(t, 20261109182156)
	pool := pgstoretest.RolePoolFromTemplate(t, fixture, admin, "detent_runtime_v2", "")
	repository, err := postgres.NewRepository(t.Context(), pool, pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := t.Context()
	organization := tracker.OrganizationID("org_reports")
	project := tracker.ProjectID("project_reports")
	now := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Minute)
	priority := 1
	state := "Todo"
	actorKind, actorID := "human", "actor_example"
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(ctx, accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return fmt.Errorf("create report organization: %w", err)
		}
		if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}}); err != nil {
			return fmt.Errorf("create report project: %w", err)
		}
		return nil
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("write report fixture failed with SQLSTATE %s on constraint %s: %v", pgErr.Code, pgErr.ConstraintName, err)
		}
		t.Fatal(err)
	}
	for i, item := range []corestore.WorkItem{
		{OrganizationID: organization, ProjectID: project, NativeID: "wi_report_bug", Number: 1, Title: "Example bug", IssueType: "bug", Priority: &priority, WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now},
		{OrganizationID: organization, ProjectID: project, NativeID: "wi_report_request", Number: 2, Title: "Example request", IssueType: "change_request", WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: now.Add(-2 * time.Minute)},
	} {
		result, err := admin.ExecContext(ctx, `WITH source_project AS (
	UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+1
	WHERE organization_id=$2 AND id=$3 AND deleted_at IS NULL
	RETURNING organization_id,id
)
INSERT INTO issues(id,organization_id,project_id,native_id,number,title,issue_type,priority,workflow_state_name,actor_kind,actor_principal_id,created_at)
SELECT public.allocate_core_record_id('public.issues_id_seq'::regclass,$1),organization_id,id,$4,$5,$6,$7,$8,$9,$10,$11,$12
FROM source_project`, int64(i+1), item.OrganizationID, item.ProjectID, item.NativeID, item.Number, item.Title, item.IssueType, item.Priority, item.WorkflowStateName, item.ActorKind, item.ActorPrincipalID, item.CreatedAt)
		if err != nil {
			t.Fatalf("seed historical report work item %s: %v", item.NativeID, err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			t.Fatalf("seed historical report work item %s: rows=%d error=%v", item.NativeID, rows, err)
		}
	}
	objects := pgstoretest.NewHistoryObjects()
	content := eventstore.NewContent(objects)
	events := []eventstore.Event{}
	for i, value := range []tracker.CollaborationEvent{
		{AggregateID: "wi_report_bug", Type: "issue.created", RecordedAt: now},
		{AggregateID: "wi_report_request", Type: "issue.created", RecordedAt: now.Add(-2 * time.Minute)},
		{AggregateID: "wi_report_bug", Type: "workflow.transitioned", RecordedAt: now.Add(10 * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Rework", ReasonDetail: "gate failed"}},
		{AggregateID: "wi_report_bug", Type: "workflow.transitioned", RecordedAt: now.Add(30 * time.Second), Data: tracker.CollaborationData{FromState: "Rework", ToState: "Done"}},
		{AggregateID: "wi_report_request", Type: "workflow.transitioned", RecordedAt: now.Add(-30 * time.Second), Data: tracker.CollaborationData{FromState: "Todo", ToState: "Done"}},
		{AggregateID: "wi_report_request", Type: "workflow.transitioned", RecordedAt: now.Add(20 * time.Second), Data: tracker.CollaborationData{FromState: "Done", ToState: "Todo"}},
	} {
		value.ID = fmt.Sprintf("report_event_%d", i)
		value.OrganizationID, value.ProjectID, value.Actor, value.SchemaVersion = organization, project, pgstoretest.HistoryActor(), 1
		event, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, value))
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RegisterMachine(ctx, leasestore.Machine{OrganizationID: organization, ID: "report_machine", Hostname: "example", Capacity: 1, Version: "example"})
	}); err != nil {
		t.Fatal(err)
	}
	var lease leasestore.Lease
	if err := admin.QueryRowContext(ctx, `WITH at AS MATERIALIZED (
	SELECT clock_timestamp() AS now
)
INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at)
SELECT $1,'report_lease',1,'report_machine','report_session',1,at.now,at.now,at.now+interval '1 hour' FROM at
RETURNING organization_id,lease_id,fencing_token,issue_id,machine_id,session_id,work_item_revision,landing_capacity,acquired_at,renewed_at,expires_at,released_at,release_reason`, organization).Scan(
		&lease.OrganizationID, &lease.LeaseID, &lease.FencingToken, &lease.IssueID, &lease.MachineID, &lease.SessionID,
		&lease.WorkItemRevision, &lease.LandingCapacity, &lease.AcquiredAt, &lease.RenewedAt, &lease.ExpiresAt,
		&lease.ReleasedAt, &lease.ReleaseReason); err != nil {
		t.Fatal(err)
	}
	run := tracker.NativeRunData{ProcessUsage: &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 8, SystemCPUSeconds: 2, WallSeconds: 10}, AttemptID: "attempt_report_failure", RunID: "run_report_failure", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "hub request timed out"}, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "merge", ResolvedModel: agentidentity.NewValue("example-model", agentidentity.ProvenanceRuntime)}, Landing: &tracker.NativeLandingReceipt{Landed: false}}}
	runRef, err := objects.StoreDetail(ctx, string(organization), pgstoretest.HistoryJSON(t, run))
	if err != nil {
		t.Fatal(err)
	}
	runEvent, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "report_run_failure", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: now.Add(40 * time.Second), Data: tracker.CollaborationData{Run: &run}}))
	if err != nil {
		t.Fatal(err)
	}
	historicalAttempt := leasestore.AttemptEvent{ID: run.AttemptID, Fence: pgstoretest.LeaseFence(lease), RunID: run.RunID, Sequence: 1, Status: "failed", StartedAt: now.Add(5 * time.Second), PayloadObjectKey: runRef.Key}
	attemptDigest := sha256.Sum256(pgstoretest.HistoryJSON(t, historicalAttempt))
	result, err := admin.ExecContext(ctx, `WITH source_lease AS MATERIALIZED (
	SELECT organization_id,issue_id,lease_id,fencing_token,work_item_revision FROM leases
	WHERE organization_id=$1 AND issue_id=$3 AND lease_id=$5 AND fencing_token=$6 AND work_item_revision=$9
	 AND released_at IS NULL AND expires_at>clock_timestamp()
	FOR UPDATE
), bound_run AS (
	INSERT INTO native_attempt_runs(organization_id,run_id,issue_id)
	SELECT organization_id,$2,issue_id FROM source_lease
	RETURNING organization_id,run_id,issue_id
), historical_attempt AS (
	INSERT INTO native_attempts
	(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,
	 dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at,
	 runtime_summary_sequence,local_attempt_id_recorded,runner_tokens_recorded)
	SELECT bound_run.organization_id,$4,bound_run.issue_id,source_lease.lease_id,source_lease.fencing_token,bound_run.run_id,
	 $7,$8,source_lease.work_item_revision,$10,$11,$12,$13,clock_timestamp(),$7,true,true
	FROM bound_run JOIN source_lease USING(organization_id,issue_id)
	RETURNING organization_id,id,sequence
)
INSERT INTO native_attempt_events(organization_id,attempt_id,sequence,request_hash)
SELECT organization_id,id,sequence,$14 FROM historical_attempt`, organization, historicalAttempt.RunID, lease.IssueID, historicalAttempt.ID,
		historicalAttempt.Fence.LeaseID, historicalAttempt.Fence.FencingToken, historicalAttempt.Sequence, historicalAttempt.Status, lease.WorkItemRevision,
		historicalAttempt.DispatchGeneration, historicalAttempt.PayloadObjectKey, historicalAttempt.StartedAt.Truncate(time.Microsecond), historicalAttempt.StartedAt.Nanosecond()%1000, fmt.Sprintf("%x", attemptDigest))
	if err != nil {
		t.Fatalf("seed historical report attempt: %v", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("seed historical report attempt: rows=%d error=%v", rows, err)
	}
	for _, event := range append(events, runEvent) {
		result, err := admin.ExecContext(ctx, `WITH issue_event AS (
	UPDATE issues SET last_activity_at=greatest(last_activity_at,$5),event_sequence=event_sequence+1
	WHERE organization_id=$1 AND project_id=$2 AND native_id=$3
	RETURNING event_sequence
), project_event AS (
	INSERT INTO project_event_cursors(organization_id,project_id,sequence)
	SELECT $1,$2,1 FROM issue_event
	ON CONFLICT (organization_id,project_id) DO UPDATE SET sequence=project_event_cursors.sequence+1
	RETURNING sequence
)
INSERT INTO collaboration_events
	(organization_id,project_id,work_item_id,id,sequence,project_sequence,type,schema_version,
	 actor_kind,actor_principal_id,actor_envelope_known,actor_via_key,actor_display_name,actor_agent,
	 event_revision,run_id,attempt_id,from_state,to_state,recorded_at,
	 payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,push_known)
SELECT $1,$2,$3,$4,issue_event.event_sequence,project_event.sequence,$6,$7,
	$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$5,$19,$20,$21,$22,$23
FROM issue_event CROSS JOIN project_event`, event.OrganizationID, event.ProjectID, event.WorkItemID, event.ID, event.RecordedAt, event.Type, event.SchemaVersion,
			event.ActorKind, event.ActorPrincipalID, event.ActorEnvelopeKnown, event.ActorViaKey, event.ActorDisplayName, event.ActorAgent,
			event.EventRevision, event.RunID, event.AttemptID, event.FromState, event.ToState,
			event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes, event.PushKnown)
		if err != nil {
			t.Fatalf("seed historical report event %s: %v", event.ID, err)
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			t.Fatalf("seed historical report event %s: rows=%d error=%v", event.ID, rows, err)
		}
	}
	for _, event := range append(events, runEvent) {
		if event.Report == nil {
			continue
		}
		p := event.Report
		if _, err := admin.ExecContext(ctx, `INSERT INTO report_event_facts
(organization_id,event_id,run_sequence,run_status,role,model,effort,failed,failure_class,failure_kind,landing_receipt,landed,landing_change_id,rework_class,rework_kind,duration_seconds)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, organization, event.ID, p.RunSequence, p.RunStatus, p.Role, p.Model, p.Effort, p.Failed, p.FailureClass, p.FailureKind, p.LandingReceipt, p.Landed, p.LandingChangeID, p.ReworkClass, p.ReworkKind, p.DurationSeconds); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO report_attempt_facts
(organization_id,attempt_id,first_event_id,latest_event_id,outcome_event_id)
VALUES ($1,$2,$3,$3,$3)`, organization, run.AttemptID, runEvent.ID); err != nil {
		t.Fatal(err)
	}

	provider, err := pgstoretest.MigrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, processEvidenceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, "RESET ROLE"); err != nil {
		t.Fatal(err)
	}
	var upgradedStatus, upgradedTiming string
	if err := admin.QueryRowContext(ctx, "SELECT process_evidence_status,phase_timing_status FROM report_event_facts WHERE organization_id=$1 AND event_id=$2", organization, runEvent.ID).Scan(&upgradedStatus, &upgradedTiming); err != nil || upgradedStatus != "unknown" || upgradedTiming != "" {
		t.Fatalf("upgrade invented historical coverage: status=%s timing=%s error=%v", upgradedStatus, upgradedTiming, err)
	}
	for _, test := range []struct {
		name string
		edit func(*eventstore.Event)
	}{
		{"object hash", func(e *eventstore.Event) { e.PayloadSHA256 = strings.Repeat("b", 64) }},
		{"object key", func(e *eventstore.Event) { e.PayloadObjectKey += "_foreign" }},
		{"project", func(e *eventstore.Event) { e.ProjectID = "foreign" }},
		{"item", func(e *eventstore.Event) { e.WorkItemID = "foreign" }},
		{"run", func(e *eventstore.Event) { e.RunID = "foreign" }},
		{"attempt", func(e *eventstore.Event) { e.AttemptID = "foreign" }},
		{"sequence", func(e *eventstore.Event) { p := *e.Report; p.RunSequence++; e.Report = &p }},
		{"timing conflicts with immutable duration", func(e *eventstore.Event) {
			p := *e.Report
			p.DurationSeconds++
			p.PhaseTimingStatus = "complete"
			e.Report = &p
		}},
	} {
		t.Run("upgrade provenance/"+test.name, func(t *testing.T) {
			bad := runEvent
			test.edit(&bad)
			if err := repository.ImportReportEventProjection(ctx, bad); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("mismatched upgrade provenance accepted: %v", err)
			}
			var status, timing string
			var peak int64
			if err := admin.QueryRowContext(ctx, "SELECT process_evidence_status,phase_timing_status,process_peak_memory_bytes FROM report_event_facts WHERE organization_id=$1 AND event_id=$2", organization, runEvent.ID).Scan(&status, &timing, &peak); err != nil || status != "unknown" || timing != "" || peak != 0 {
				t.Fatalf("failed upgrade changed evidence: %s %s %d %v", status, timing, peak, err)
			}
		})
	}
	for _, mode := range []string{"event replay", "import replay"} {
		if mode == "import replay" {
			if _, err := admin.ExecContext(ctx, `UPDATE report_event_facts SET phase_timing_status='',process_evidence_status='unknown',process_peak_memory_bytes=0,process_user_cpu_seconds=0,process_system_cpu_seconds=0,process_wall_seconds=0 WHERE organization_id=$1 AND event_id=$2`, organization, runEvent.ID); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "event replay" {
			if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, runEvent); return err }); err != nil {
				t.Fatal(err)
			}
		} else if err := repository.ImportReportEventProjection(ctx, runEvent); err != nil {
			t.Fatal(err)
		}
		var status, timing string
		var peak int64
		if err := admin.QueryRowContext(ctx, `SELECT process_evidence_status,phase_timing_status,process_peak_memory_bytes FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, organization, runEvent.ID).Scan(&status, &timing, &peak); err != nil {
			t.Fatal(err)
		}
		if status != "usable" || peak != 4096 || timing != "partial" {
			t.Fatalf("%s lost verified process projection: status=%s timing=%s peak=%d", mode, status, timing, peak)
		}
	}
	var landedLease leasestore.Lease
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Release(ctx, pgstoretest.LeaseFence(lease), "completed"); err != nil {
			return err
		}
		var err error
		landedLease, err = w.Claim(ctx, leasestore.Claim{IssueID: 1, ExpectedRevision: 1, MachineID: "report_machine", SessionID: "report_landed_session", LeaseID: "report_landed_lease", TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	landedRun := tracker.NativeRunData{AttemptID: "attempt_report_landed", RunID: "run_report_landed", LeaseID: tracker.LeaseID(landedLease.LeaseID), FencingToken: tracker.FencingToken(landedLease.FencingToken), Sequence: 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "merge", ResolvedModel: agentidentity.NewValue("other-model", agentidentity.ProvenanceRuntime)}, Landing: &tracker.NativeLandingReceipt{Landed: true, ChangeID: "change_report", VersionID: "version_report"}}}
	landedRef, err := objects.StoreDetail(ctx, string(organization), pgstoretest.HistoryJSON(t, landedRun))
	if err != nil {
		t.Fatal(err)
	}
	landedEvent, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "report_run_landed", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: now.Add(50 * time.Second), Data: tracker.CollaborationData{Run: &landedRun}}))
	if err != nil {
		t.Fatal(err)
	}
	change, err := content.PrepareChange(ctx, organization, pgstoretest.HistoryJSON(t, tracker.ChangeRequest{ID: "change_report", OrganizationID: organization, ProjectID: project, WorkItemID: "wi_report_bug", Title: "Example landed change", Revision: 1, CreatedAt: now, UpdatedAt: now.Add(50 * time.Second), Landed: &tracker.ChangeLanding{VersionID: "version_report", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40), BaseRef: "develop", LandedAt: now.Add(50 * time.Second)}}))
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareVersion(ctx, organization, pgstoretest.HistoryJSON(t, tracker.ChangeVersion{ID: "version_report", ChangeID: "change_report", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("c", 40), Repository: "owner/repository", PolicyID: "policy_example", AttemptID: landedRun.AttemptID}, Actor: pgstoretest.HistoryActor(), CreatedAt: now}))
	if err != nil {
		t.Fatal(err)
	}
	escapeEvidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: organization, ChangeID: "change_report", Kind: "discussion"}, pgstoretest.HistoryJSON(t, tracker.ChangeDiscussion{ID: "discussion_report_escape", VersionID: "version_report", Actor: pgstoretest.HistoryActor(), CreatedAt: now.Add(56 * time.Second), Escape: &tracker.QualityEscape{OccurrenceID: "occurrence_report", Kind: "scheduled_failure", Cause: "validator_miss", ObservedAt: now.Add(55 * time.Second), HumanOverride: true}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: landedRun.AttemptID, Fence: pgstoretest.LeaseFence(landedLease), RunID: landedRun.RunID, Sequence: 1, Status: "succeeded", StartedAt: now.Add(45 * time.Second), PayloadObjectKey: landedRef.Key}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, landedEvent); err != nil {
			return err
		}
		if _, err := w.CreateChange(ctx, change); err != nil {
			return err
		}
		if _, err := w.CreateVersion(ctx, version); err != nil {
			return err
		}
		stored, err := w.AppendEvidence(ctx, escapeEvidence)
		escapeEvidence.Sequence = stored.Sequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repository.ImportReportEscapeProjection(ctx, escapeEvidence); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{"attempt_report_failure", "attempt_report_landed"} {
		record := reportstore.UsageRecord{OrganizationID: organization, UsageFact: reportstore.UsageFact{ProjectID: project, WorkItemID: "wi_report_bug", AttemptID: id, Period: now, Provider: "example", Model: "example-model", Input: 10, CachedInput: 12, Output: 3, Cost: 1.25, Currency: "usd"}, UpdatedAt: now}
		for range 2 {
			if err := repository.ImportReportUsageFact(ctx, record); err != nil {
				t.Fatal(err)
			}
		}
		record.Cost = 2
		if err := repository.ImportReportUsageFact(ctx, record); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("conflicting usage: %v", err)
		}
	}
	err = repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.WorkItem(ctx, project, "wi_report_bug")
		if err != nil {
			return err
		}
		if stored.IssueType != "bug" || stored.Priority == nil || *stored.Priority != 1 {
			t.Fatalf("stored report metadata = type %q priority %v", stored.IssueType, stored.Priority)
		}
		persisted, err := r.WorkItems(ctx, project, 0, 10)
		if err != nil {
			return err
		}
		if len(persisted) != 2 {
			t.Fatalf("persisted report fixture count = %d; first created at %s", len(persisted), stored.CreatedAt)
		}

		usageReader := r.(reportstore.UsageReader)
		usageQuery := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Limit: 10}
		usage, err := usageReader.ReadReportUsage(ctx, usageQuery)
		if err != nil {
			return err
		}
		if len(usage) != 2 || usage[0].Cost+usage[1].Cost != 2.5 || usage[0].CachedInput != 12 {
			t.Fatalf("typed usage = %+v", usage)
		}
		usageQuery.Principal = "another_actor"
		hidden, err := usageReader.ReadReportUsage(ctx, usageQuery)
		if err != nil {
			return err
		}
		if len(hidden) != 0 {
			t.Fatal("usage leaked across principal scope")
		}
		usageQuery.Principal = ""
		usageQuery.Limit = 1
		if _, err := usageReader.ReadReportUsage(ctx, usageQuery); !errors.Is(err, reportstore.ErrPopulationExceeded) {
			t.Fatalf("usage overflow: %v", err)
		}
		reader, ok := r.(reportstore.Reader)
		if !ok {
			t.Fatal("tenant reader does not implement report reads")
		}
		allFacts, err := reader.ReadOverviewFacts(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(time.Minute), Limit: 10})
		if err != nil {
			return err
		}
		if len(allFacts) != 2 {
			t.Fatalf("unfiltered report facts = %+v", allFacts)
		}
		for _, fact := range allFacts {
			if fact.WorkItemID == "wi_report_request" && (fact.CompletedAt != nil || fact.PreviousCompletedAt == nil || fact.PreviousCompletedState != "Done") {
				t.Fatalf("reopened issue lost prior-period completion: %+v", fact)
			}
		}
		facts, err := reader.ReadOverviewFacts(ctx, reportstore.OverviewQuery{
			Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(time.Minute), Limit: 10,
			Filter: reportstore.Filter{Types: []string{"bug"}, Priorities: []int{1}},
		})
		if err != nil {
			return err
		}
		if len(facts) != 1 || facts[0].WorkItemID != "wi_report_bug" || facts[0].CreatedAt.IsZero() {
			t.Fatalf("filtered report facts = %+v", facts)
		}
		attemptReader, ok := r.(reportstore.AttemptReader)
		if !ok {
			t.Fatal("tenant reader does not implement report attempt reads")
		}
		attempts, err := attemptReader.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Limit: 10})
		if err != nil {
			return err
		}
		if len(attempts) != 2 || !attempts[0].ProjectionKnown || attempts[0].Role != "merge" || attempts[0].Model != "example-model" || attempts[0].FailureClass != "Hub request timed out" || attempts[0].FailureKind != "infrastructure" {
			t.Fatalf("report attempt facts = %+v", attempts)
		}
		openReader, ok := r.(reportstore.OpenReader)
		if !ok {
			t.Fatal("tenant reader does not implement open report reads")
		}
		openFacts, err := openReader.ReadOpenFacts(ctx, reportstore.OpenQuery{Projects: []tracker.ProjectID{project}, At: now.Add(time.Hour), Limit: 10})
		if err != nil {
			return err
		}
		if len(openFacts) != 1 || openFacts[0].State != "Todo" || openFacts[0].Terminal || openFacts[0].EnteredAt == nil {
			t.Fatalf("typed current open facts = %+v", openFacts)
		}
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), Split: now, To: now.Add(2 * time.Minute), Limit: 10}
		history, err := r.(reportstore.HistoryReader).ReadHistory(ctx, query)
		if err != nil {
			return err
		}
		if len(history) != 1 || len(history[0].Issues) != 2 || len(history[0].Issues[0].Events) != 2 || history[0].Issues[0].Events[0].Class.Name != "Gate failed before landing" {
			t.Fatalf("typed report history = %+v", history)
		}
		landings, err := r.(reportstore.LandingReader).ReadLandingFacts(ctx, query)
		if err != nil {
			return err
		}
		if len(landings) != 1 || landings[0].Attempts != 2 || !landings[0].Complete {
			t.Fatalf("typed landing attempts = %+v", landings)
		}
		escapes, err := r.(reportstore.QualityReader).ReadEscapeFacts(ctx, query)
		if err != nil {
			return err
		}
		if len(escapes) != 1 || escapes[0].OccurrenceID != "occurrence_report" || escapes[0].Cause != "validator_miss" || !escapes[0].HumanOverride {
			t.Fatalf("typed report escape facts = %+v", escapes)
		}
		facets, err := reader.ReadFacets(ctx, []tracker.ProjectID{project}, query.From, query.To, "", 10)
		if err != nil {
			return err
		}
		if len(facets.FiledBy) != 1 || facets.FiledBy[0].Name != "actor_example" || len(facets.Models) != 2 || facets.Models[0] != "example-model" || len(facets.Stages) != 1 || facets.Stages[0] != "merge" {
			t.Fatalf("typed facets = %+v", facets)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("lifetime shipped spend includes earlier periods only for selected work", func(t *testing.T) {
		ctx := t.Context()
		err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(reportstore.SummaryUsageReader)
			if !ok {
				return errors.New("typed summary usage owner missing")
			}
			from, to := now.Add(time.Second), now.Add(time.Minute)
			for _, check := range []struct {
				name, principal string
				lifetime        []tracker.NativeWorkItemID
				count           int
				cost            float64
			}{
				{name: "window excludes earlier spend"},
				{name: "shipped item includes earlier spend", principal: actorID, lifetime: []tracker.NativeWorkItemID{"wi_report_bug"}, count: 2, cost: 2.5},
				{name: "unselected item cannot include earlier spend", lifetime: []tracker.NativeWorkItemID{"wi_report_request"}},
				{name: "another principal cannot include earlier spend", principal: "another_actor", lifetime: []tracker.NativeWorkItemID{"wi_report_bug"}},
			} {
				t.Run(check.name, func(t *testing.T) {
					rows, err := reader.ReadSummaryUsage(ctx, project, check.principal, from, to, check.lifetime)
					if err != nil {
						t.Fatal(err)
					}
					var cost float64
					for _, row := range rows {
						if row.WorkItemID != "wi_report_bug" || !row.Period.Equal(now) {
							t.Fatalf("lifetime usage row outside the expected item and period: %+v", row)
						}
						cost += row.Cost
					}
					if len(rows) != check.count || cost != check.cost {
						t.Fatalf("lifetime usage = %d rows, %.2f, want %d rows, %.2f", len(rows), cost, check.count, check.cost)
					}
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("first landing try uses typed attempt roles and receipts", func(t *testing.T) {
		rollback := errors.New("rollback report fixture")
		for _, check := range []struct {
			name, role string
			receipt    *tracker.NativeLandingReceipt
			attempts   int
			complete   bool
		}{
			{name: "earlier implementation does not count", role: "implement", attempts: 1, complete: true},
			{name: "receipt-backed merge refusal counts", role: "merge", receipt: &tracker.NativeLandingReceipt{ChangeID: "change_report", RefusalKind: "conflict", ObservedAt: now.Add(40 * time.Second)}, attempts: 2, complete: true},
			{name: "earlier merge without a receipt makes the count incomplete", role: "merge", attempts: 2},
		} {
			t.Run(check.name, func(t *testing.T) {
				ctx := t.Context()
				prior := run
				prior.Sequence++
				runtime := *run.Runtime
				runtime.Identity.Role, runtime.Landing = check.role, check.receipt
				prior.Runtime = &runtime
				event, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{
					ID: "report_prior_attempt_role", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1,
					Actor: pgstoretest.HistoryActor(), RecordedAt: now.Add(40 * time.Second), Data: tracker.CollaborationData{Run: &prior},
				}))
				if err != nil {
					t.Fatal(err)
				}
				err = repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.AppendEvent(ctx, event); err != nil {
						return err
					}
					reader, ok := w.(reportstore.LandingReader)
					if !ok {
						return errors.New("typed report landing reader missing")
					}
					window := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(45 * time.Second), To: now.Add(time.Minute), Limit: 10}
					rows, err := reader.ReadLandingFacts(ctx, window)
					if err != nil {
						return err
					}
					if len(rows) != 1 || rows[0].ChangeID != "change_report" || rows[0].Attempts != check.attempts || rows[0].Complete != check.complete {
						t.Fatalf("typed landing = %+v, want %d attempts and complete=%t", rows, check.attempts, check.complete)
					}
					return rollback
				})
				if !errors.Is(err, rollback) {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("same attempt population for usage model filters", func(t *testing.T) {
		event, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "report_model_update", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: now.Add(2 * time.Hour), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: landedRun.AttemptID, RunID: landedRun.RunID, Sequence: 2, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "implement", ResolvedModel: agentidentity.NewValue("other-model", agentidentity.ProvenanceRuntime)}}}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, event); return err }); err != nil {
			t.Fatal(err)
		}
		for _, filter := range []reportstore.Filter{{Models: []string{"missing-model"}}, {Stages: []string{"missing-stage"}}, {Runners: []string{"missing-runner"}}, {Models: []string{"example-model"}}} {
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				q := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Minute), To: now.Add(time.Minute), Filter: filter, Limit: 10}
				attempts, err := r.(reportstore.AttemptReader).ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: q.Projects, From: q.From, To: q.To, Filter: filter, Limit: 10})
				if err != nil {
					return err
				}
				usage, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, q)
				if err != nil {
					return err
				}
				selected := map[string]bool{}
				for _, a := range attempts {
					selected[a.AttemptID] = true
				}
				for _, u := range usage {
					if !selected[u.AttemptID] {
						t.Fatalf("unscoped usage: %+v", u)
					}
				}
				if len(usage) != len(attempts) {
					t.Fatalf("attempts=%d usage=%d filter=%+v", len(attempts), len(usage), filter)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("immutable outcome time survives late update", func(t *testing.T) {
		correction, err := content.PrepareEvent(ctx, organization, pgstoretest.HistoryJSON(t, tracker.CollaborationEvent{ID: "report_failure_correction", OrganizationID: organization, ProjectID: project, AggregateID: "wi_report_bug", Type: "run.finished", SchemaVersion: 1, Actor: pgstoretest.HistoryActor(), RecordedAt: now.Add(2 * time.Hour), Data: tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: run.AttemptID, RunID: run.RunID, Sequence: 2, Outcome: "succeeded"}}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(ctx, correction); return err }); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `DELETE FROM report_attempt_facts WHERE organization_id=$1 AND attempt_id=$2`, organization, run.AttemptID); err != nil {
			t.Fatal(err)
		}
		for _, event := range []eventstore.Event{correction, runEvent} {
			if err := repository.ImportReportEventProjection(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := admin.ExecContext(ctx, `UPDATE native_attempts SET updated_at=$1,status='succeeded' WHERE organization_id=$2 AND id=$3`, now.Add(24*time.Hour), organization, run.AttemptID); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.AttemptReader).ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: []tracker.ProjectID{project}, From: now, To: now.Add(time.Minute), Limit: 10})
			if err != nil {
				return err
			}
			for _, a := range rows {
				if a.AttemptID == run.AttemptID {
					if !a.Failed || a.Status != "failed" || !a.OutcomeAt.Equal(now.Add(40*time.Second)) {
						t.Fatalf("historical outcome changed: %+v", a)
					}
					return nil
				}
			}
			t.Fatal("historical failure disappeared")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("coverage requires verified import or current accepted revisions", func(t *testing.T) {
		query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: now.Add(-time.Hour), To: now.Add(time.Hour), Limit: 100}
		assertCoverage := func(want bool) {
			t.Helper()
			if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				got, err := r.(reportstore.UsageCoverageReader).ReadReportUsageCoverage(ctx, query)
				if err != nil {
					return err
				}
				if got != want {
					t.Fatalf("coverage=%v want %v", got, want)
				}
				costs, err := r.(reportstore.CompletionCostReader).ReadCompletionCosts(ctx, reportstore.CompletionCostQuery{
					Items:     []reportstore.CompletionCostItem{{ProjectID: project, WorkItemID: "wi_report_bug"}, {ProjectID: project, WorkItemID: "wi_report_request"}},
					Principal: actorID, CurrentTo: now.Add(time.Hour), PriorTo: now.Add(-time.Hour),
				})
				if err != nil {
					return err
				}
				if len(costs) != 2 || costs[0].WorkItemID != "wi_report_bug" || costs[0].Complete != want || costs[1].WorkItemID != "wi_report_request" || !costs[1].Complete || costs[1].Cost != 0 || costs[1].PriorCost != 0 {
					t.Fatalf("completion coverage=%+v want complete=%t and a known empty second item", costs, want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		assertCoverage(false)
		for _, id := range []string{run.AttemptID, landedRun.AttemptID} {
			if err := repository.VerifyReportUsageImport(ctx, organization, id, strings.Repeat("a", 64), 1); err != nil {
				t.Fatal(err)
			}
		}
		assertCoverage(true)
		t.Run("completion cost cutoffs and principal scope", func(t *testing.T) {
			for _, test := range []struct {
				name               string
				currentTo, priorTo time.Time
				cost, priorCost    float64
			}{
				{"upper cutoff excluded", now, now.Add(-time.Hour), 0, 0},
				{"current includes earlier spend", now.Add(time.Microsecond), now, 2.5, 0},
				{"prior lifetime spend", now.Add(2 * time.Microsecond), now.Add(time.Microsecond), 2.5, 2.5},
			} {
				t.Run(test.name, func(t *testing.T) {
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						query := reportstore.CompletionCostQuery{Items: []reportstore.CompletionCostItem{{ProjectID: project, WorkItemID: "wi_report_bug"}}, Principal: actorID, CurrentTo: test.currentTo, PriorTo: test.priorTo}
						rows, err := r.(reportstore.CompletionCostReader).ReadCompletionCosts(t.Context(), query)
						if err != nil {
							return err
						}
						if len(rows) != 1 || !rows[0].Complete || rows[0].Cost != test.cost || rows[0].PriorCost != test.priorCost {
							t.Fatalf("completion costs=%+v want current%.2f prior%.2f complete", rows, test.cost, test.priorCost)
						}
						query.Principal = "foreign_actor"
						rows, err = r.(reportstore.CompletionCostReader).ReadCompletionCosts(t.Context(), query)
						if err != nil {
							return err
						}
						if len(rows) != 0 {
							t.Fatalf("completion costs crossed principal scope: %+v", rows)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
		makeRecord := func(revision int64, amount int64, coverage string) billingstore.CostObservation {
			record := billingCostObservation(t, organization, project, "report_current", run.AttemptID, "runner_api", now.Add(-time.Minute), now, amount)
			o := &record.Observation
			o.AttemptID = run.AttemptID
			o.WorkItemID = "wi_report_bug"
			o.Model = "example-model"
			o.Input = 100
			o.CachedInput = 50
			o.Output = 10
			o.Coverage = coverage
			o.Revision = revision
			record.ReceivedAt = now
			raw, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			record.Reference = billingstore.AuditReferenceFromArchive(ref)
			return record
		}
		first := makeRecord(1, 2000000, "complete")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{first})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(false)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first, first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		probe := pgstoretest.ReportObjectReadProbe{DetailStore: objects, Check: func() {
			if repository.Stats().AcquiredConns() != 0 {
				t.Fatal("object read held target transaction")
			}
		}}
		after, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, "", 1)
		if err != nil || count != 1 || after == "" {
			t.Fatalf("refresh page count=%d cursor=%q err=%v", count, after, err)
		}
		if _, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, after, 1); err != nil || count != 0 {
			t.Fatalf("refresh page continuation count=%d err=%v", count, err)
		}
		assertCoverage(true)
		corrected := makeRecord(2, 3000000, "partial")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{corrected}); err != nil {
				return err
			}
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first, corrected, corrected, first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(false)
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, query)
			if err != nil {
				return err
			}
			total := 0.0
			for _, u := range rows {
				total += u.Cost
			}
			if total != 4.25 {
				t.Fatalf("corrected usage duplicated historical data: %v", total)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		zero := makeRecord(3, 0, "complete")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{zero}); err != nil {
				return err
			}
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{zero})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{first})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			rows, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, query)
			if err != nil {
				return err
			}
			for _, u := range rows {
				if u.AttemptID == run.AttemptID && u.Cost != 0 {
					t.Fatalf("explicit zero replaced by stale usage: %+v", u)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		second := makeRecord(1, 5000000, "complete")
		second.Observation.SourceID = "report_second"
		second.Observation.From, second.Observation.To = now.Add(-2*time.Minute), now.Add(-time.Minute)
		raw, err := json.Marshal(second.Observation)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := objects.StoreDetail(ctx, string(organization), raw)
		if err != nil {
			t.Fatal(err)
		}
		second.Reference = billingstore.AuditReferenceFromArchive(ref)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{second})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(false)
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.(reportstore.UsageWriter).RefreshReportUsage(ctx, []billingstore.CostObservation{second})
		}); err != nil {
			t.Fatal(err)
		}
		assertCoverage(true)

		classified := make([]billingstore.CostObservation, 0, 2)
		for i, kind := range []string{"cumulative", "incremental"} {
			record := makeRecord(1, 6000000, "complete")
			o := &record.Observation
			o.SourceID = "classified-" + kind
			o.UsageKind = kind
			o.Model = "classification-model"
			o.From, o.To = now.Add(-time.Duration(6+i*2)*time.Minute), now.Add(-time.Duration(5+i*2)*time.Minute)
			if i == 0 {
				o.Currency = "EUR"
			}
			raw, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(ctx, string(organization), raw)
			if err != nil {
				t.Fatal(err)
			}
			record.Reference = billingstore.AuditReferenceFromArchive(ref)
			if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{record})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			after, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, "", 100)
			if err != nil || count != i+3 || after == "" {
				t.Fatalf("classified refresh count=%d cursor=%q error=%v", count, after, err)
			}
			var currency, storedKind string
			if err := admin.QueryRowContext(ctx, `SELECT u.currency,u.usage_kind FROM report_usage_current u JOIN cost_observation_revisions r ON r.organization_id=u.organization_id AND r.source_key=u.source_key AND r.revision=u.revision WHERE u.organization_id=$1 AND r.object_key=$2`, organization, ref.Key).Scan(&currency, &storedKind); err != nil {
				t.Fatal(err)
			}
			if currency != o.Currency || storedKind != kind {
				t.Fatalf("immutable classification=%s/%s want=%s/%s", currency, storedKind, o.Currency, kind)
			}
			classified = append(classified, record)
		}
		hourly := reportstore.UsageRecord{OrganizationID: organization, UsageFact: reportstore.UsageFact{ProjectID: project, WorkItemID: "wi_report_bug", AttemptID: run.AttemptID, Period: now.UTC().Truncate(time.Hour), Provider: classified[0].Observation.Provider, Model: "classification-model", Input: 7, Output: 3, Cost: 0.5, Currency: "USD"}, UpdatedAt: now}
		if err := repository.ImportReportUsageFact(ctx, hourly); err != nil {
			t.Fatal(err)
		}
		result, err := admin.ExecContext(ctx, `UPDATE attempt_usage SET usage_sequence=1,usage_origin='current' WHERE organization_id=$1 AND attempt_id=$2 AND period=$3 AND provider=$4 AND model=$5 AND currency='USD'`, organization, run.AttemptID, hourly.Period, hourly.Provider, hourly.Model)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("historical current-shape rows=%d error=%v", n, err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			facts, err := r.(reportstore.UsageReader).ReadReportUsage(ctx, query)
			if err != nil {
				return err
			}
			total := 0.0
			for _, fact := range facts {
				if fact.AttemptID == run.AttemptID && fact.Model == "classification-model" {
					total += fact.Cost
				}
			}
			if total != 6.5 {
				t.Fatalf("hourly cumulative projection hid incremental source: %v want6.5", total)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var sourceKey string
		if err := admin.QueryRowContext(ctx, `SELECT source_key FROM cost_observation_revisions WHERE organization_id=$1 AND object_key=$2 AND revision=1`, organization, classified[0].Reference.Key).Scan(&sourceKey); err != nil {
			t.Fatal(err)
		}
		replace := func(callCtx context.Context, ref billingstore.AuditReference) error {
			result, err := admin.ExecContext(callCtx, `WITH changed AS (UPDATE cost_observation_revisions SET binding_id=$3,customer_owned=$4,object_key=$5,sha256=$6,compressed_bytes=$7,content_bytes=$8,codec=$9,format_version=$10,kind=$11,retention=$12 WHERE organization_id=$1 AND source_key=$2 AND revision=1 RETURNING source_key) UPDATE cost_sources SET payload_hash=$6 WHERE organization_id=$1 AND source_key IN(SELECT source_key FROM changed)`, organization, sourceKey, ref.BindingID, ref.CustomerOwned, ref.Key, ref.SHA256, ref.CompressedBytes, ref.ContentBytes, ref.Codec, ref.FormatVersion, ref.Kind, ref.Retention)
			if err != nil {
				return err
			}
			if n, err := result.RowsAffected(); err != nil {
				return err
			} else if n != 1 {
				return fmt.Errorf("archive substitution rows=%d", n)
			}
			return nil
		}
		if err := replace(ctx, classified[1].Reference); err != nil {
			t.Fatal(err)
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := replace(cleanup, classified[0].Reference); err != nil {
				t.Error(err)
			}
		}()
		if after, count, err := repository.RefreshReportUsagePage(ctx, organization, probe, "", 100); err == nil || after != "" || count != 0 {
			t.Fatalf("mismatched immutable identity advanced refresh: cursor=%q count=%d error=%v", after, count, err)
		}

	})
	t.Run("facet input overflow fails even with one distinct filer", func(t *testing.T) {
		flood := tracker.ProjectID("project_reports_flood")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: flood, Name: "Example flood", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,actor_kind,actor_principal_id) SELECT $1,10000+n,$2,'wi_facet_'||n,10000+n,'Example','human','actor_flood' FROM generate_series(1,5001) n`, organization, flood); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{flood}, now.Add(-time.Hour), now.Add(time.Hour), "", 10)
			if !errors.Is(err, reportstore.ErrPopulationExceeded) {
				t.Fatalf("facet source overflow: %v", err)
			}
			for _, principal := range []string{"", actorID} {
				facets, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{project}, now.Add(-time.Hour), now.Add(time.Hour), principal, 10)
				if err != nil {
					t.Fatalf("unrelated project facets principal=%q failed after flood: %v", principal, err)
				}
				if len(facets.FiledBy) != 1 || facets.FiledBy[0].ID != actorID {
					t.Fatalf("unrelated project filers principal=%q = %+v", principal, facets.FiledBy)
				}
			}
			if _, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{project, flood}, now.Add(-time.Hour), now.Add(time.Hour), actorID, 10); err != nil {
				t.Fatalf("principal-scoped facets failed after another filer's flood: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("facets read the window and open issues, not retained history", func(t *testing.T) {
		history := tracker.ProjectID("project_reports_facet_history")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: history, Name: "Example history", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		old := now.Add(-400 * 24 * time.Hour)
		if _, err := admin.ExecContext(ctx, `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name,archived,created_at,native_created_at,native_updated_at,terminal_entered_at) VALUES
($1,30001,$2,'wi_facet_closed',30001,'Example','human','actor_closed','Done',true,$3,$3,$3,$3),
($1,30002,$2,'wi_facet_open',30002,'Example','human','actor_open','Todo',false,$3,$3,$3,NULL),
($1,30003,$2,'wi_facet_recent',30003,'Example','human','actor_recent','Done',false,$4,$4,$4,$4)`, organization, history, old, now); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			facets, err := r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{history}, now.Add(-time.Hour), now.Add(time.Hour), "", 10)
			if err != nil {
				return err
			}
			got := []string{}
			for _, filer := range facets.FiledBy {
				got = append(got, filer.ID)
			}
			if !slices.Equal(got, []string{"actor_open", "actor_recent"}) {
				t.Fatalf("facet filers=%v want the open and in-window issues only", got)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("facet label rows stop at the candidate sentinel", func(t *testing.T) {
		labeled := tracker.ProjectID("project_reports_facet_labels")
		if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: labeled, Name: "Example labels", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issues(organization_id,id,project_id,native_id,number,title,actor_kind,actor_principal_id,workflow_state_name,archived,created_at,native_created_at,native_updated_at) SELECT $1,40000+n,$2,'wi_facet_label_'||n,40000+n,'Example','human','actor_labels','Todo',false,$3,$3,$3 FROM generate_series(1,5000) n`, organization, labeled, now); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issue_labels(organization_id,project_id,work_item_id,label) SELECT $1,$2,'wi_facet_label_'||n,'label_'||k FROM generate_series(1,5000) n CROSS JOIN generate_series(1,10) k`, organization, labeled); err != nil {
			t.Fatal(err)
		}
		read := func() (reportstore.Facets, error) {
			var facets reportstore.Facets
			err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				facets, err = r.(reportstore.Reader).ReadFacets(ctx, []tracker.ProjectID{labeled}, now.Add(-time.Hour), now.Add(time.Hour), "", 11)
				return err
			})
			return facets, err
		}
		facets, err := read()
		if err != nil || len(facets.Labels) != 10 || len(facets.FiledBy) != 1 {
			t.Fatalf("50,000 label rows: labels=%v filers=%v error=%v", facets.Labels, facets.FiledBy, err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issue_labels(organization_id,project_id,work_item_id,label) VALUES($1,$2,'wi_facet_label_1','label_11')`, organization, labeled); err != nil {
			t.Fatal(err)
		}
		facets, err = read()
		if !errors.Is(err, reportstore.ErrPopulationExceeded) || len(facets.Labels) != 0 || len(facets.FiledBy) != 0 {
			t.Fatalf("50,001st label row: labels=%v filers=%v error=%v, want ErrPopulationExceeded and no options", facets.Labels, facets.FiledBy, err)
		}
	})

}
