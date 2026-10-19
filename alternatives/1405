package hubserver

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func BenchmarkAnalyticsRollupReads(b *testing.B) {
	f := benchmarkAnalyticsFixture(b, 50000)
	now := f.service.config.now()
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour} {
		w := operatortool.AnalyticsWindow{From: now.Add(-span), To: now, Bucket: time.Hour}
		for _, source := range []struct {
			name string
			q    nativeQueryer
		}{{"raw", f.service.database.reader}, {"rollups", analyticsRollupReader{f.service.database.reader}}} {
			for _, endpoint := range []string{"diagnostics", "reports"} {
				b.Run(fmt.Sprintf("%s/%s/%dh", source.name, endpoint, int(span.Hours())), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						project, err := readNativeAnalytics(b.Context(), source.q, scope, operatortool.AnalyticsRequest{Limit: 1000}, w)
						if err != nil {
							b.Fatal(err)
						}
						report := nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{project}}
						if endpoint == "diagnostics" {
							_, err = f.service.readHostedDiagnosticsFrom(b.Context(), report, source.q)
						} else {
							_, err = f.service.readHostedReportsFrom(b.Context(), report, source.q)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkAnalyticsReportSubreads(b *testing.B) {
	f := benchmarkAnalyticsFixture(b, 0)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	now := formatHubTime(f.service.config.now())
	if _, err := f.service.database.db.ExecContext(b.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('machine_browser_preview_runner','example-runner',1,'test',?,?,?)`, now, now, now); err != nil {
		b.Fatal(err)
	}
	seedProductionProject(b, f.service, scope, pyroApexShape)
	for _, span := range []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		w := operatortool.AnalyticsWindow{From: f.service.config.now().Add(-span), To: f.service.config.now(), Bucket: time.Hour}
		for _, source := range []struct {
			name string
			q    nativeQueryer
		}{{"raw", f.service.database.reader}, {"rollups", analyticsRollupReader{f.service.database.reader}}} {
			for _, read := range []struct {
				name string
				run  func() error
			}{
				{"attempts", func() error { _, err := loadNativeAnalyticsAttempts(b.Context(), source.q, scope, w, ""); return err }},
				{"quality", func() error {
					_, err := readAnalyticsQuality(b.Context(), source.q, scope, operatortool.AnalyticsRequest{Limit: maxAnalyticsPopulation}, w)
					return err
				}},
				{"landings", func() error { _, err := nativeAnalyticsLandings(b.Context(), source.q, scope, w); return err }},
			} {
				b.Run(fmt.Sprintf("%s/%s/%dh", source.name, read.name, int(span.Hours())), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := read.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

func BenchmarkAnalyticsRollupAppend(b *testing.B) {
	for _, projected := range []bool{false, true} {
		b.Run(fmt.Sprintf("projected=%t", projected), func(b *testing.B) {
			f := benchmarkAnalyticsFixture(b, 0)
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			var id string
			if err := f.service.database.reader.QueryRowContext(b.Context(), "SELECT native_id FROM issues WHERE organization_id=? AND project_id=?", scope.organization, scope.project).Scan(&id); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				tx, err := f.service.database.db.BeginTx(b.Context(), nil)
				if err != nil {
					b.Fatal(err)
				}
				if err := appendNativeHistory(b.Context(), tx, scope, id, "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "ready"}}, f.service.config.now()); err != nil {
					b.Fatal(err)
				}
				err = f.service.database.commit(b.Context(), tx)
				if err == nil && projected {
					_, err = f.service.database.drainNativeBoardDirty(b.Context(), f.service.config.now())
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkAnalyticsFixture(b *testing.B, events int) nativeFixture {
	b.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s, err := Open(b.Context(), Config{DatabasePath: filepath.Join(b.TempDir(), "hub.db"), InitialAdminToken: []byte(testHubAdminToken), Logger: discardLogger(), now: func() time.Time { return now }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	})
	tx, err := s.database.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{project: "benchmark-project"}
	if err := tx.QueryRowContext(b.Context(), "SELECT id FROM organizations WHERE local=1").Scan(&scope.organization); err != nil {
		b.Fatal(err)
	}
	if _, err := tx.ExecContext(b.Context(), `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) VALUES(?,?,'benchmark','native','[]',?)`, scope.project, scope.organization, formatHubTime(now)); err != nil {
		b.Fatal(err)
	}
	if err := applyNativeProjectStates(b.Context(), tx, scope, nativeFixtureStates(), now); err != nil {
		b.Fatal(err)
	}
	issue, err := createNativeFixtureIssueTx(b.Context(), tx, scope, tracker.CreateIssue{Title: "benchmark", State: "Todo"}, now.Add(-8*24*time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	if events > 0 {
		if _, err := tx.ExecContext(b.Context(), `WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at)
SELECT 'benchmark-event-'||x,?,?,?,x+1,CASE WHEN x%100=0 THEN 'workflow.transitioned' ELSE 'scheduler.decision' END,1,'{}',
CASE WHEN x%100=0 THEN json_object('from_state',CASE WHEN (x/100)%2=1 THEN 'Todo' ELSE 'In Progress' END,'to_state',CASE WHEN (x/100)%2=1 THEN 'In Progress' ELSE 'Todo' END) ELSE json_object('decision',json_object('source','native_provider_capacity','outcome','skipped','reason','capacity')) END,
strftime('%Y-%m-%dT%H:%M:%SZ',?,'+'||(x*12)||' seconds') FROM n`, events, scope.organization, scope.project, issue.WorkItemID, formatHubTime(now.Add(-7*24*time.Hour))); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.ExecContext(b.Context(), "UPDATE issues SET event_sequence=event_sequence+? WHERE native_id=?", events, issue.WorkItemID); err != nil {
			b.Fatal(err)
		}
	}
	if err := s.database.commit(b.Context(), tx); err != nil {
		b.Fatal(err)
	}
	return nativeFixture{service: s, project: tracker.NativeProject{OrganizationID: scope.organization, ID: scope.project}}
}
