package billingrecords_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHistoryCheckpointReportParity(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	const project = "example-history"
	const item = "example-item"
	orgs := []tracker.OrganizationID{"history_first", "history_second"}
	for n, org := range orgs {
		pgstoretest.SeedRepository(t, repository, org)
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "History", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "In Progress", Dispatchable: true}, {Name: "Rework", Dispatchable: true}, {Name: "Done", Terminal: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO issues(organization_id,project_id,native_id,number,title,workflow_state_name,created_at,native_updated_at) VALUES ($1,$2,$3,1,'Example','Done',$4,$5)`, org, project, item, at.AddDate(0, -3, 0), at); err != nil {
			t.Fatal(err)
		}
		count := 20484
		if n == 1 {
			count = 1029
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT $1,$2,$3,'history_'||n,n*CASE WHEN $6::boolean THEN 2 ELSE 128 END,CASE WHEN n%6=5 THEN 'scheduler.decision' ELSE 'workflow.transitioned' END,1,
 (ARRAY['Rework','Todo','In Progress','Done','Rework','Rework'])[1+n%6],(ARRAY['Todo','In Progress','Done','Rework','Rework','Rework'])[1+n%6],
 CASE WHEN n>$5-4 THEN $4::timestamptz+(n-$5)*interval '1 hour'
 WHEN $6::boolean AND n>512 THEN $4::timestamptz-n*interval '1 minute'
 ELSE $4::timestamptz-interval '75 days'+(n/2)*interval '1 minute' END,
 'orgs/'||$1||'/blobs/sha256/aa/'||repeat('a',64),repeat('a',64),1,1 FROM generate_series(1,$5::integer) n`, org, project, item, at, count, n == 0); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `INSERT INTO report_event_facts(organization_id,event_id,rework_class,rework_kind)
 SELECT organization_id,id,'Infrastructure','infrastructure' FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND to_state='Rework' AND id<>'history_3'`, org, project); err != nil {
			t.Fatal(err)
		}
	}
	check := func(org tracker.OrganizationID, window time.Duration, wantCheckpoint bool) {
		t.Helper()
		w := operatortool.AnalyticsWindow{From: at.Add(-window), To: at, Bucket: window / 8}
		prior := operatortool.AnalyticsWindow{From: w.From.Add(-window), To: w.From, Bucket: w.Bucket}
		if err := repository.ReadTenantSnapshot(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			actual, err := r.(reportstore.HistoryReader).ReadHistory(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: prior.From, Split: w.From, To: w.To, Limit: 5000})
			if err != nil {
				return err
			}
			if len(actual) != 1 || len(actual[0].Issues) != 1 {
				t.Fatalf("history population for %s: %+v", org, actual)
			}
			issue := actual[0].Issues[0]
			if (issue.Omitted > 0) != wantCheckpoint {
				t.Fatalf("%s %v omitted %d events", org, window, issue.Omitted)
			}
			raw := reportstore.HistoryIssue{CreatedAt: issue.CreatedAt, InitialState: issue.InitialState, WorkItemID: issue.WorkItemID}
			cursor := reportstore.HistoryEventCursor{Through: 132000}
			for {
				page, err := r.(reportstore.HistoryIssuePageReader).ReadHistoryIssueEvents(ctx, project, item, cursor, 200)
				if err != nil {
					return err
				}
				raw.Events = append(raw.Events, slices.DeleteFunc(page.Events, func(e reportstore.WorkflowEvent) bool { return !e.At.Before(w.To) })...)
				if page.Exhausted {
					break
				}
				cursor.After = page.After
			}
			oracle := []reportstore.ProjectHistory{{ProjectID: project, States: actual[0].States, Issues: []reportstore.HistoryIssue{raw}}}
			render := func(history []reportstore.ProjectHistory) []byte {
				out := reports.SummarizePostgresHostedReport(nil, nil, nil, true, true, true, []string{project}, w, prior, true)
				reports.ApplyPostgresReportHistory(&out, history, nil, w, prior)
				reports.ApplyPostgresReportQuality(&out, history, []reportstore.LandingFact{{ChangeID: "change", WorkItemID: item, VersionID: "version", At: at.AddDate(0, -2, 0)}}, nil, nil, w, prior)
				reports.MarkPostgresReportCoverage(&out, history, nil)
				data, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			got, want := render(actual), render(oracle)
			if !bytes.Equal(got, want) {
				i := 0
				for i < min(len(got), len(want)) && got[i] == want[i] {
					i++
				}
				t.Fatalf("%s %v byte %d checkpoint %s raw %s", org, window, i, got[max(0, i-60):min(len(got), i+80)], want[max(0, i-60):min(len(want), i+80)])
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range orgs {
		pgstoretest.RefreshReportCheckpoints(t, repository, org)
	}
	for _, org := range orgs {
		for _, window := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour} {
			check(org, window, true)
		}
	}
	if err := repository.ReadTenantSnapshot(ctx, orgs[0], corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.(reportstore.HistoryReader).ReadHistory(ctx, reportstore.OverviewQuery{Projects: []tracker.ProjectID{project}, From: at.Add(-48 * time.Hour), To: at, Limit: 204})
		return err
	}); !errors.Is(err, reportstore.ErrPopulationExceeded) {
		t.Fatalf("20,483 events with a 20,400 event limit: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT organization_id,project_id,work_item_id,'history_new_tail',sequence+1,type,schema_version,'Done','Rework',$2::timestamptz-interval '30 minutes',payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 AND id='history_1029'`, orgs[1], at); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `UPDATE report_checkpoint_cursors SET legacy_complete=true WHERE organization_id=$1 AND project_id=$2`, orgs[1], project); err != nil {
		t.Fatal(err)
	}
	check(orgs[1], 24*time.Hour, true)

	if _, err := admin.ExecContext(ctx, `INSERT INTO report_event_facts(organization_id,event_id,rework_class,rework_kind) VALUES ($1,'history_3','Infrastructure','infrastructure')`, orgs[1]); err != nil {
		t.Fatal(err)
	}
	check(orgs[1], 24*time.Hour, false)
	pgstoretest.RefreshReportCheckpoints(t, repository, orgs[1])
	check(orgs[1], 24*time.Hour, true)

	if _, err := admin.ExecContext(ctx, `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,from_state,to_state,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes)
 SELECT organization_id,project_id,work_item_id,'history_late',sequence-1,type,schema_version,'Todo','Done',recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes FROM collaboration_events WHERE organization_id=$1 AND id='history_13'`, orgs[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `UPDATE report_checkpoint_cursors SET legacy_complete=true WHERE organization_id=$1 AND project_id=$2`, orgs[1], project); err != nil {
		t.Fatal(err)
	}

	check(orgs[1], 24*time.Hour, false)
	pgstoretest.RefreshReportCheckpoints(t, repository, orgs[1])
	check(orgs[1], 24*time.Hour, true)
	if err := repository.WriteTenant(ctx, orgs[1], corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(ctx, project)
		if err != nil {
			return err
		}
		for i := range record.States {
			if record.States[i].Name == "Todo" {
				record.States[i].Dispatchable = false
			}
		}
		_, err = w.UpdateProjectRecord(ctx, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	check(orgs[1], 24*time.Hour, false)
	pgstoretest.RefreshReportCheckpoints(t, repository, orgs[1])
	check(orgs[1], 24*time.Hour, true)
	check(orgs[0], 24*time.Hour, true)
}
