package hubserver

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAnalyticsTypedProjectionMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	prior := fstest.MapFS{}
	for _, file := range files {
		if file.Name() >= "20261025231000_" {
			continue
		}
		raw, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		prior[file.Name()] = &fstest.MapFile{Data: raw}
	}
	if _, err := runMigrationsFromFS(t.Context(), db, prior, discardLogger(), hubGoMigrations(discardLogger())); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO organizations(id,name,created_at) VALUES('org_example','Example organization','2026-10-25T00:00:00Z')`,
		`INSERT INTO projects(id,organization_id,name,profile,created_at) VALUES('project-a','org_example','Project A','native','2026-10-25T00:00:00Z'),('project-b','org_example','Project B','native','2026-10-25T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	const payload = `{"work_item_id":"item","sequence":7,"type":"scheduler.decision","recorded_at":"2026-10-25T00:00:00.000000007Z","started_at":"2026-10-25T00:00:00Z","updated_at":"2026-10-25T01:00:00Z","attempt_id":"attempt","period":"2026-10-25T00:00:00Z","ID":"sample","At":"2026-10-25T01:00:00Z","AtNS":1792890000000000000,"StartNS":1792886400000000000,"FromNS":1792886400000000001,"Source":"sample-source","Reason":"sample-reason","source":"source","reason":"refused","outcome":"skipped","to_state":"Merging","last_at":"2026-10-25T01:00:00Z","last_ns":1792890000000000000,"id":"issue","native_id":"item","title":"Example item","created_at":"2026-10-25T00:00:00Z","workflow_state_id":"state","lease_id":"lease","status":"failed","provider":"example-provider","model":"example-model","input":100,"cached_input":20,"output":10,"currency":"USD","acquired_at":"2026-10-25T00:00:00Z","released_at":null,"expires_at":"2026-10-25T02:00:00Z","failed_at":"2026-10-25T01:00:00Z","identity":{"role":"rework"},"runtime":{"identity":{"role":"code"}}}`
	for i := range storageConversionRows + 2 {
		project := "project-a"
		if i%2 != 0 {
			project = "project-b"
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES('org_example',?,0,'capacity_claim',?,1,?)`, project, fmt.Sprintf("legacy-%03d", i), payload); err != nil {
			t.Fatal(err)
		}
	}
	identities := []struct{ name, value string }{
		{"html", "<>&"},
		{"unicode", "\u2028\u2029雪"},
		{"escapes", "\"\\\n\t\b\f\r\x00\x01\x1f\\u2028\\u003c"},
	}
	projects := []string{"project-a", "project-b"}
	at := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, project := range projects {
		for _, identity := range identities {
			attempt := project + "-" + identity.name
			if _, err := db.ExecContext(t.Context(), `INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES(?,'org_example',?,?,'example-provider',?,10,2,3,1.5,'USD',?)`, attempt, project, formatHubTime(at), identity.value, formatHubTime(at)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
SELECT organization_id,project_id,unixepoch(period),'usage',json_array(attempt_id,period,provider,model,currency),cost_estimate,json_object('attempt_id',attempt_id,'period',period,'provider',provider,'model',model,'input',input,'cached_input',cached_input,'output',output,'currency',currency,'updated_at',updated_at) FROM attempt_usage WHERE attempt_id=?`, attempt); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES('org_example',?,?,'skipped',json_array(?,?),1,json_object('source',?,'reason',?,'last_at',?,'last_ns',?))`, project, at.Unix(), "source"+identity.value, "reason"+identity.value, "source"+identity.value, "reason"+identity.value, formatHubTime(at), at.UnixNano()); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json) VALUES('org_example',?,?,'skip_sample',?,1,json_object('Source',?,'Reason',?,'At',?,'AtNS',?))`, project, at.Unix(), attempt, "source"+identity.value, "reason"+identity.value, formatHubTime(at), at.UnixNano()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := runMigrations(t.Context(), db, discardLogger()); err != nil {
		t.Fatal(err)
	}
	d := &database{db: db, reader: db}
	converted := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM analytics_hourly_rollups WHERE projection_version=1").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	served := func(project string) bool {
		t.Helper()
		q, err := analyticsProjectReader(t.Context(), analyticsRollupReader{db}, nativeScope{organization: "org_example", project: tracker.ProjectID(project)})
		if err != nil {
			t.Fatal(err)
		}
		_, projected := q.(analyticsRollupReader)
		return projected
	}
	if converted() != 0 || served("project-a") || served("project-b") {
		t.Fatal("startup rewrote history or an unknown projection selected the rollup reader")
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_projection BEFORE UPDATE ON analytics_hourly_rollups WHEN NEW.dimension='legacy-001' BEGIN SELECT RAISE(ABORT,'conversion unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.convertAnalyticsProjections(t.Context()); err == nil || converted() != 1 {
		t.Fatalf("one failed row rolled back an earlier row: converted=%d error=%v", converted(), err)
	}
	if _, err := db.ExecContext(t.Context(), "DROP TRIGGER reject_projection"); err != nil {
		t.Fatal(err)
	}
	if err := d.convertAnalyticsProjections(t.Context()); err != nil {
		t.Fatal(err)
	}
	if converted() != storageConversionRows+1 || served("project-b") {
		t.Fatalf("conversion exceeded its tick budget or served unknown rows: %d", converted())
	}
	if err := d.convertAnalyticsProjections(t.Context()); err != nil {
		t.Fatal(err)
	}
	if converted() != storageConversionRows+2+3*len(projects)*len(identities) || !served("project-a") || !served("project-b") {
		t.Fatal("bounded conversion did not finish both projects")
	}
	columns := `work_item_id,sequence,type,recorded_at,started_at,updated_at,attempt_id,period,sample_id,sample_at,sample_ns,sample_start_ns,sample_from_ns,skip_source,skip_reason,last_at,last_ns,issue_id,native_id,title,created_at,workflow_state_id,lease_id,status,provider,model,input,cached_input,output,currency,acquired_at,released_at,expires_at,failed_at,scheduler_outcome,scheduler_source,scheduler_reason,analytics_to_state,attempt_role`
	want := []any{"item", int64(7), "scheduler.decision", "2026-10-25T00:00:00.000000007Z", "2026-10-25T00:00:00Z", "2026-10-25T01:00:00Z", "attempt", "2026-10-25T00:00:00Z", "sample", "2026-10-25T01:00:00Z", int64(1792890000000000000), int64(1792886400000000000), int64(1792886400000000001), "sample-source", "sample-reason", "2026-10-25T01:00:00Z", int64(1792890000000000000), "issue", "item", "Example item", "2026-10-25T00:00:00Z", "state", "lease", "failed", "example-provider", "example-model", int64(100), int64(20), int64(10), "USD", "2026-10-25T00:00:00Z", nil, "2026-10-25T02:00:00Z", "2026-10-25T01:00:00Z", "skipped", "source", "refused", "Merging", "code"}
	rows, err := db.QueryContext(t.Context(), "SELECT "+columns+",data_json FROM analytics_hourly_rollups WHERE metric='capacity_claim'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		got := make([]any, len(want)+1)
		args := make([]any, len(got))
		for i := range got {
			args[i] = &got[i]
		}
		if err := rows.Scan(args...); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[:len(want)], want) || got[len(want)] != payload {
			t.Fatalf("typed fields or opaque payload changed: %v", got)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var objects int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE (name LIKE 'analytics_%' OR tbl_name='analytics_hourly_rollups') AND lower(sql) LIKE '%json_extract%'`).Scan(&objects); err != nil || objects != 0 {
		t.Fatalf("JSON analytics schema objects remain: %d %v", objects, err)
	}
	for _, project := range projects {
		t.Run("usage refresh/"+project, func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), `UPDATE attempt_usage SET input=25,cached_input=5,output=7,cost_estimate=2.5 WHERE project_id=?`, project); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := refreshAnalyticsUsage(t.Context(), tx, "org_example", project, "", 0); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			for _, identity := range identities {
				var count, input, cached, output int
				var cost float64
				var dimension string
				if err := db.QueryRowContext(t.Context(), `SELECT count(*),sum(input),sum(cached_input),sum(output),sum(value),dimension FROM analytics_hourly_rollups WHERE organization_id='org_example' AND project_id=? AND metric='usage' AND model=?`, project, identity.value).Scan(&count, &input, &cached, &output, &cost, &dimension); err != nil {
					t.Fatal(err)
				}
				var historical string
				if err := db.QueryRowContext(t.Context(), `SELECT json_array(?,?,?,?,'USD')`, project+"-"+identity.name, formatHubTime(at), "example-provider", identity.value).Scan(&historical); err != nil {
					t.Fatal(err)
				}
				if count != 1 || input != 25 || cached != 5 || output != 7 || cost != 2.5 || dimension != historical {
					t.Errorf("%s: usage refresh changed identity or totals: rows=%d input=%d cached=%d output=%d cost=%g dimension=%q want %q", identity.name, count, input, cached, output, cost, dimension, historical)
				}
			}
		})
	}
	t.Run("skip compaction", func(t *testing.T) {
		window := operatortool.AnalyticsWindow{From: at, To: at.Add(time.Hour), Bucket: time.Hour}
		before := map[string]nativeAnalyticsProject{}
		for _, project := range projects {
			out := nativeAnalyticsProject{Window: window}
			ok, err := readAnalyticsSkipRollups(t.Context(), db, nativeScope{organization: "org_example", project: tracker.ProjectID(project)}, &out, false)
			if err != nil || !ok || out.DecisionsObserved != len(identities) {
				t.Fatalf("%s: historical skips=%+v ok=%v error=%v", project, out.SkipReasons, ok, err)
			}
			before[project] = out
		}
		remaining := func() int {
			t.Helper()
			var count int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE metric IN ('capacity_claim','skip_sample')`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count
		}
		for count := remaining(); count > 0; {
			if err := d.compactAnalyticsSamples(t.Context(), at.Add(analyticsSampleRetention+2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			next := remaining()
			if next >= count {
				t.Fatalf("compaction made no progress: before=%d after=%d", count, next)
			}
			count = next
		}
		for _, project := range projects {
			var samples int
			if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE project_id=? AND metric='skip_sample'`, project).Scan(&samples); err != nil || samples != 0 {
				t.Fatalf("%s: historical samples remaining=%d error=%v", project, samples, err)
			}
			out := nativeAnalyticsProject{Window: window}
			ok, err := readAnalyticsSkipRollups(t.Context(), db, nativeScope{organization: "org_example", project: tracker.ProjectID(project)}, &out, true)
			if err != nil || !ok || out.DecisionsObserved != before[project].DecisionsObserved || !reflect.DeepEqual(out.SkipReasons, before[project].SkipReasons) {
				t.Fatalf("%s: compacted skips=%+v before=%+v error=%v", project, out.SkipReasons, before[project].SkipReasons, err)
			}
		}
	})
}

func TestAnalyticsFailureTime(t *testing.T) {
	observed := time.Date(2026, 10, 25, 0, 0, 0, 7, time.UTC)
	updated := "2026-10-25T01:00:00Z"
	for _, test := range []struct {
		name, status string
		landing      *analyticsAttemptLanding
		want         string
	}{
		{"unknown landing", "succeeded", nil, ""},
		{"failed without landing", "failed", nil, updated},
		{"interrupted", "interrupted", nil, updated},
		{"conflict", "succeeded", &analyticsAttemptLanding{RefusalKind: "conflict", ObservedAt: observed}, formatHubTime(observed)},
		{"protection", "succeeded", &analyticsAttemptLanding{RefusalKind: "base_protected", ObservedAt: observed}, formatHubTime(observed)},
		{"refusal then landed", "succeeded", &analyticsAttemptLanding{RefusalKind: "conflict", Landed: true, ObservedAt: observed}, updated},
		{"gate failure", "succeeded", &analyticsAttemptLanding{GateFailed: true, Landed: true, ObservedAt: observed}, formatHubTime(observed)},
		{"missing observation", "succeeded", &analyticsAttemptLanding{GateFailed: true}, updated},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := analyticsFailureTime(test.landing, test.status, updated)
			if got.Valid != (test.want != "") || got.String != test.want || strings.Contains(got.String, "0001-") {
				t.Fatalf("failure time=%v want %q", got, test.want)
			}
		})
	}
}

func TestAnalyticsEventProjectionUnknown(t *testing.T) {
	const refusal = "refused<&>\u2028\u2029"
	now := time.Date(2026, 10, 25, 1, 0, 0, 7, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
	for _, fixture := range []nativeFixture{f, other} {
		issue := fixture.create(t, "Example item")
		scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range []struct {
			kind string
			data tracker.CollaborationData
		}{
			{"workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}},
			{"scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Source: "native_claim", Reason: refusal, RunnerID: "runner"}}},
			{"run.started", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "attempt-" + string(issue.WorkItemID)}}},
		} {
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), event.kind, event.data, now); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var compatible bool
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT analytics_skip_key=json_array('native_claim',?) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'`, refusal, issue.WorkItemID).Scan(&compatible); err != nil || !compatible {
			t.Fatalf("new event changed historical skip key: compatible=%v error=%v", compatible, err)
		}
		ids := []string{string(issue.WorkItemID)}
		want, err := readShippedResidenceIssues(t.Context(), f.service.database.reader, scope, ids, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET analytics_projection_version=NULL,analytics_from_state=NULL,analytics_to_state=NULL,analytics_attempt_id=NULL,analytics_skip_key=NULL,analytics_recorded_ns=NULL,scheduler_outcome=NULL,scheduler_source='',scheduler_reason='',scheduler_runner_id='' WHERE organization_id=? AND project_id=?`, scope.organization, scope.project); err != nil {
			t.Fatal(err)
		}
		unknown, err := readShippedResidenceIssues(t.Context(), f.service.database.reader, scope, ids, now.Add(time.Hour))
		if err != nil || !reflect.DeepEqual(unknown, want) {
			t.Fatalf("unknown events changed residence: got=%v want=%v error=%v", unknown, want, err)
		}
		if err := f.service.convertSchedulerDecisions(t.Context()); err != nil {
			t.Fatal(err)
		}
		var outcome, reason, source, attempt string
		var ns int64
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT scheduler_outcome,scheduler_reason,scheduler_source,analytics_recorded_ns,analytics_skip_key=json_array('native_claim',?) FROM collaboration_events WHERE work_item_id=? AND type='scheduler.decision'`, refusal, issue.WorkItemID).Scan(&outcome, &reason, &source, &ns, &compatible); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT analytics_attempt_id FROM collaboration_events WHERE work_item_id=? AND type='run.started'`, issue.WorkItemID).Scan(&attempt); err != nil {
			t.Fatal(err)
		}
		if outcome != "skipped" || reason != refusal || source != "native_claim" || ns != now.UnixNano() || attempt != "attempt-"+string(issue.WorkItemID) || !compatible {
			t.Fatalf("event projection lost refusal or source facts: %s %s %s %d %s", outcome, reason, source, ns, attempt)
		}
		got, err := readShippedResidenceIssues(t.Context(), f.service.database.reader, scope, ids, now.Add(time.Hour))
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("backfilled events changed residence: got=%v want=%v error=%v", got, want, err)
		}
	}
}

func TestAnalyticsCompactionLegacyIdentifiers(t *testing.T) {
	for _, test := range []struct{ name, id string }{{"number", "9007199254740993"}, {"string", `"9007199254740993"`}} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			d := f.service.database
			var badIDs []int64
			for _, fixture := range []nativeFixture{f, other} {
				scope := nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID}
				insert := func(metric, dimension, raw string, version any) int64 {
					t.Helper()
					result, err := d.db.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,?,?,?,2,?,?)`, scope.organization, scope.project, now.Unix(), metric, dimension, raw, version)
					if err != nil {
						t.Fatal(err)
					}
					id, err := result.LastInsertId()
					if err != nil {
						t.Fatal(err)
					}
					return id
				}
				badIDs = append(badIDs, insert("issue", "bad", `{"id":{"title":"private customer content"}}`, nil))
				insert("issue", "good", fmt.Sprintf(`{"id":%s,"workflow_state_id":%s}`, test.id, test.id), nil)
				insert("lease", "legacy", fmt.Sprintf(`{"issue_id":%s,"released_at":"2026-10-25T00:00:00Z"}`, test.id), nil)
				badIDs = append(badIDs, insert("capacity_claim", "bad-sample", `{"At":"private customer content"}`, 1))
				badIDs = append(badIDs, insert("residence_hour", "bad-residence", `{"At":"private customer content"}`, 1))
				insert("residence_hour", "good-residence", `{"State":"Done"}`, 1)
				insert("slots", "good-slot", `{}`, nil)
			}
			err := d.compactAnalyticsSamples(t.Context(), now.Add(analyticsSampleRetention+2*time.Hour))
			if err == nil || strings.Contains(err.Error(), "private customer content") {
				t.Fatalf("unsafe or missing row error: %v", err)
			}
			for _, id := range badIDs {
				if !strings.Contains(err.Error(), fmt.Sprintf("rowid=%d", id)) {
					t.Fatalf("missing identity %d: %v", id, err)
				}
			}
			for _, fixture := range []nativeFixture{f, other} {
				var id, state string
				if err := d.reader.QueryRowContext(t.Context(), `SELECT issue_id,workflow_state_id FROM analytics_hourly_rollups WHERE project_id=? AND dimension='good'`, fixture.project.ID).Scan(&id, &state); err != nil {
					t.Fatal(err)
				}
				if id != "9007199254740993" || state != id {
					t.Fatalf("lost identifier precision: %s %s", id, state)
				}
				var remaining, count int
				var total float64
				if err := d.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE project_id=? AND metric IN ('slots','lease')`, fixture.project.ID).Scan(&remaining); err != nil {
					t.Fatal(err)
				}
				if remaining != 0 {
					t.Fatalf("valid rows did not compact: %d", remaining)
				}
				if err := d.reader.QueryRowContext(t.Context(), `SELECT count,total FROM analytics_sample_hours WHERE project_id=? AND metric='slots'`, fixture.project.ID).Scan(&count, &total); err != nil {
					t.Fatal(err)
				}
				if count != 1 || total != 2 {
					t.Fatalf("compacted totals: %d %g", count, total)
				}
				if err := d.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE project_id=? AND dimension IN ('bad','bad-sample','bad-residence')`, fixture.project.ID).Scan(&remaining); err != nil {
					t.Fatal(err)
				}
				if remaining != 3 {
					t.Fatalf("malformed data lost: %d", remaining)
				}
				var through int64
				if err := d.reader.QueryRowContext(t.Context(), `SELECT through_hour FROM analytics_residence_retention WHERE project_id=? AND work_item_id='good-residence'`, fixture.project.ID).Scan(&through); err != nil {
					t.Fatal(err)
				}
				if through != now.Add(2*time.Hour).Unix() {
					t.Fatalf("valid residence did not progress: %d", through)
				}
			}
		})
	}
}

func TestAnalyticsMaintenanceFailureBatchProgress(t *testing.T) {
	for _, test := range []struct {
		name, metric string
		rows         int
		version      any
		clockStep    time.Duration
	}{
		{"projection", "slots", storageConversionRows, nil, 0},
		{"samples", "capacity_claim", analyticsSampleCompactionBatch, 1, 0},
		{"residence", "residence_hour", analyticsSampleCompactionBatch, 1, 0},
		{"time budget", "capacity_claim", 1, 1, analyticsBackfillBudget + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
			f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			d := f.service.database
			tx, err := d.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := range test.rows {
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,?,?,?,2,?,?)`, f.project.OrganizationID, f.project.ID, now.Unix(), test.metric, fmt.Sprintf("bad-%05d", i), `{"At":{"value":"private customer content"}}`, test.version); err != nil {
					t.Fatal(err)
				}
			}
			goodMetric := "slots"
			if test.metric == "residence_hour" {
				goodMetric = test.metric
			}
			for _, fixture := range []nativeFixture{f, other} {
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,?,?, 'good',2,?,?)`, fixture.project.OrganizationID, fixture.project.ID, now.Unix(), goodMetric, `{"State":"Done"}`, test.version); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			maintenance := func() {
				t.Helper()
				restarted := &database{db: d.db, reader: d.reader}
				elapsed := time.Duration(0)
				err := restarted.compactAnalyticsSamplesWithClock(t.Context(), now.Add(analyticsSampleRetention+time.Hour), func() time.Time {
					at := now.Add(elapsed)
					elapsed += test.clockStep
					return at
				})
				if err != nil && (!strings.Contains(err.Error(), "rowid=") || strings.Contains(err.Error(), "private customer content")) {
					t.Fatalf("unexpected or unsafe diagnostic: %v", err)
				}
			}
			completed := func(project, dimension string) bool {
				t.Helper()
				var done bool
				query := `SELECT NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE project_id=? AND dimension=?)`
				if test.metric == "residence_hour" {
					query = `SELECT EXISTS(SELECT 1 FROM analytics_residence_retention WHERE project_id=? AND work_item_id=? AND through_hour>0)`
				}
				if err := d.reader.QueryRowContext(t.Context(), query, project, dimension).Scan(&done); err != nil {
					t.Fatal(err)
				}
				return done
			}
			maintenance()
			if completed(string(other.project.ID), "good") {
				t.Fatal("fixture did not exceed the first pass budget")
			}
			for pass := 0; pass < test.rows+3 && !completed(string(other.project.ID), "good"); pass++ {
				maintenance()
			}
			for _, fixture := range []nativeFixture{f, other} {
				if !completed(string(fixture.project.ID), "good") {
					t.Fatal("valid rows behind a full failed batch did not compact")
				}
			}
			var retained int
			if err := d.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM analytics_hourly_rollups WHERE dimension LIKE 'bad-%'`).Scan(&retained); err != nil || retained != test.rows {
				t.Fatalf("failed rows retained=%d want=%d: %v", retained, test.rows, err)
			}
			if _, err := d.db.ExecContext(t.Context(), `UPDATE analytics_hourly_rollups SET data_json='{"State":"Done"}' WHERE project_id=? AND dimension='bad-00000'`, f.project.ID); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < test.rows+3 && !completed(string(f.project.ID), "bad-00000"); pass++ {
				maintenance()
			}
			if !completed(string(f.project.ID), "bad-00000") {
				t.Fatal("repaired row was never retried")
			}
		})
	}
}
