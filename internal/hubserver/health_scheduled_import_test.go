package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthScheduledNeutralImportPreservesHistory(t *testing.T) {
	objects := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: objects, runMaintenance: waitForMaintenanceFixtureShutdown})
	base := time.Date(2026, 10, 1, 0, 0, 0, 123456789, time.UTC)
	persistedBase := base.Truncate(time.Microsecond)
	const repository = "example-org/example-project"
	var sources []HealthScheduledImportSource
	fixtures := map[tracker.OrganizationID]*coreNeutralFixture{}
	expected := map[tracker.OrganizationID]map[tracker.ProjectID][]healthstore.ScheduledReport{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		fixture := newCoreNeutralFixture(t, org, "projects", "repositories", "scheduled_validation_reports", "issues", "native_comments")
		fixtures[org] = fixture
		fixture.add("repositories", map[string]any{"id": 1, "github_owner": "example-org", "github_name": "example-project"})
		expected[org] = map[tracker.ProjectID][]healthstore.ScheduledReport{}
		var projects []tracker.ProjectID
		for projectIndex, name := range []string{"First scheduled history", "Second scheduled history"} {
			f := newNativeFixture(t, service, org, name)
			projects = append(projects, f.project.ID)
			sourceProject := map[string]any{"id": f.project.ID, "checkout_repository": repository}
			if projectIndex == 1 {
				sourceProject["checkout_repository"], sourceProject["repository_id"] = "", 1
			}
			fixture.add("projects", sourceProject)
			report := func(run, conclusion string, hour int) healthstore.ScheduledReport {
				return healthstore.ScheduledReport{OrganizationID: org, ProjectID: f.project.ID, RecordedAt: base.Add(time.Duration(hour) * time.Hour), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: repository, RunID: run, RunAttempt: "1", RunURL: "https://github.com/" + repository + "/actions/runs/" + run + "/attempts/1", HeadSHA: strings.Repeat("a", 40), Conclusion: conclusion}}
			}
			for _, value := range []healthstore.ScheduledReport{report("1", "success", 0), report("2", "failure", 1), report("5", "failure", 4), report("6", "skipped", 10)} {
				rawValue := value.ScheduledValidationReport
				var typedConclusion any
				if value.RunID == "2" {
					rawValue.Conclusion = "skipped"
					typedConclusion = value.Conclusion
				}
				raw, err := json.Marshal(rawValue)
				if err != nil {
					t.Fatal(err)
				}
				fixture.add("scheduled_validation_reports", map[string]any{"project_id": f.project.ID, "repository": repository, "run_id": value.RunID, "run_attempt": "1", "record_json": string(raw), "recorded_at": value.RecordedAt.Format(time.RFC3339Nano), "conclusion": typedConclusion})
				expected[org][f.project.ID] = append(expected[org][f.project.ID], value)
			}
			failure := func(run string) string {
				value := report(run, "failure", 0)
				evidence := gate.ScheduledEvidence{Schema: 1, Repository: repository, OccurrenceKey: strings.Repeat("b", 64), RunID: run, RunAttempt: "1", JobID: "1", HeadSHA: value.HeadSHA, Conclusion: "failure", RunURL: value.RunURL}
				return issueorigin.Stamp("Retained scheduled failure", issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: strings.TrimSuffix(value.RunURL, "/attempts/1"), Fingerprint: "scheduled-job"}) + evidence.Stamp()
			}
			green := func(run string) string {
				value := report(run, "success", 0)
				return "Scheduled full validation passed for development commit `" + value.HeadSHA + "`.\n\nRun: " + value.RunURL + "\n\nRetained evidence."
			}
			for index, row := range []struct {
				table, body, rawActor string
				kind                  any
				hour                  int
			}{
				{"issues", green("11"), `{"kind":"human"}`, "human", 10},
				{"issues", failure("3"), `{"kind":"runner"}`, "human", 2},
				{"native_comments", failure("3"), `{"kind":"human"}`, nil, 4},
				{"native_comments", green("4"), `{"kind":"human"}`, nil, 3},
				{"issues", failure("1"), `{"kind":"human"}`, "human", -1},
				{"native_comments", green("7"), `{"kind":"human"}`, "runner", 6},
				{"issues", failure("8"), `{"kind":"runner"}`, nil, 7},
				{"native_comments", strings.ReplaceAll(failure("9"), "example-org/example-project", "other-org/other-project"), `{"kind":"human"}`, "human", 8},
				{"issues", strings.Replace(failure("10"), "github-actions", "another-instance", 1), `{"kind":"human"}`, "human", 9},
			} {
				id := any(fmt.Sprintf("comment_%d_%d", projectIndex, index))
				if row.table == "native_comments" && row.hour == 3 {
					id = fmt.Sprintf("zz_green_%d", projectIndex)
				}
				if row.table == "issues" {
					id = projectIndex*100 + index + 1
				}
				fixture.add(row.table, map[string]any{"id": id, "project_id": f.project.ID, "body": row.body, "actor_kind": row.kind, "actor_json": row.rawActor, "created_at": base.Add(time.Duration(row.hour) * time.Hour).Format(time.RFC3339Nano)})
			}
			for index := range 101 {
				fixture.add("native_comments", map[string]any{"id": fmt.Sprintf("middle_%d_%03d", projectIndex, index), "project_id": f.project.ID, "body": failure("3"), "actor_kind": "human", "actor_json": `{"kind":"human"}`, "created_at": base.Add(4 * time.Hour).Format(time.RFC3339Nano)})
			}
			expected[org][f.project.ID] = append(expected[org][f.project.ID], report("3", "failure", 2), report("4", "success", 3))
		}
		path := fixture.write()
		for _, project := range projects {
			sources = append(sources, HealthScheduledImportSource{BundlePath: path, OrganizationID: org, ProjectID: project, Objects: objects})
		}
	}
	for _, projects := range expected {
		for _, reports := range projects {
			for index := range reports {
				reports[index].RecordedAt = reports[index].RecordedAt.Truncate(time.Microsecond)
			}
		}
	}
	for _, source := range sources {
		source.DryRun = true
		count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, source)
		if err != nil || count != 6 {
			t.Fatalf("dry run count=%d error=%v", count, err)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), source.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
			history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), source.ProjectID, base.Add(11*time.Hour))
			if len(history.Reports) != 0 || !history.FirstReport.IsZero() || !history.LastGreen.IsZero() {
				t.Fatalf("dry run empty history=%+v error=%v", history, err)
			}
			_, receiptErr := r.Receipt(t.Context(), "production-import", "neutral-source", "source")
			if !errors.Is(receiptErr, corestore.ErrNotFound) {
				t.Fatalf("dry run pinned source: %v", receiptErr)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*coreNeutralFixture)
	}{
		{"malformed report", func(f *coreNeutralFixture) {
			f.records["scheduled_validation_reports"][0].Values["record_json"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("{")}
		}},
		{"typed identity mismatch", func(f *coreNeutralFixture) {
			f.records["scheduled_validation_reports"][0].Values["record_json"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(`{"repository":"example-org/example-project","run_id":"90","run_attempt":"1","run_url":"https://github.com/example-org/example-project/actions/runs/90/attempts/1","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","conclusion":"success"}`)}
		}},
		{"invalid original time", func(f *coreNeutralFixture) {
			f.records["scheduled_validation_reports"][0].Values["recorded_at"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("invalid")}
		}},
		{"foreign evidence", func(f *coreNeutralFixture) {
			f.records["native_comments"][0].Values["organization_id"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("org_foreign")}
		}},
		{"malformed legacy actor", func(f *coreNeutralFixture) {
			f.records["native_comments"][0].Values["actor_json"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("{")}
		}},
		{"schema drift", func(f *coreNeutralFixture) {
			table := f.tables["scheduled_validation_reports"]
			table.Columns["unmapped"] = `[8,"unmapped","TEXT",1,null,0,0]`
			f.tables["scheduled_validation_reports"] = table
			for _, row := range f.records["scheduled_validation_reports"] {
				row.Values["unmapped"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("retain")}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := fixtures["org_first"]
			fixture := &coreNeutralFixture{t: t, organization: original.organization, tables: maps.Clone(original.tables), records: map[string][]postgres.NeutralImportRow{}}
			for name, table := range fixture.tables {
				table.Columns = maps.Clone(table.Columns)
				fixture.tables[name] = table
			}
			for name, rows := range original.records {
				for _, row := range rows {
					row.Key, row.Values = slices.Clone(row.Key), maps.Clone(row.Values)
					fixture.records[name] = append(fixture.records[name], row)
				}
			}
			test.mutate(fixture)
			source := sources[0]
			source.BundlePath = fixture.write()
			if count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, source); err == nil || count != 0 {
				t.Fatalf("invalid import count=%d error=%v", count, err)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), source.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
				history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), source.ProjectID, base.Add(11*time.Hour))
				if len(history.Reports) != 0 {
					t.Fatal("invalid import wrote scheduled history")
				}
				_, receiptErr := r.Receipt(t.Context(), "production-import", "neutral-source", "source")
				if !errors.Is(receiptErr, corestore.ErrNotFound) {
					t.Fatalf("invalid import pinned source: %v", receiptErr)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	foreign := sources[0]
	foreign.OrganizationID = sources[2].OrganizationID
	if count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, foreign); err == nil || count != 0 {
		t.Fatalf("foreign source count=%d error=%v", count, err)
	}
	foreign = sources[0]
	foreign.ProjectID = sources[2].ProjectID
	if count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, foreign); err == nil || count != 0 {
		t.Fatalf("foreign project count=%d error=%v", count, err)
	}
	for _, stage := range []string{"import", "replay", "reopen"} {
		if stage == "reopen" {
			cfg := service.config
			cfg.Hosted = &HostedConfig{OrganizationID: "org_first", WorkOSOrganizationID: "provider_org_first", PublicURL: "https://hub.example.test", Provider: newHostedSecurityProvider()}
			if err := service.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			service = reopened
			t.Cleanup(func() { _ = reopened.Close() })
			service.config.Hosted = nil
			service.echo = echo.New()
			service.echo.Use(service.measureRequests, service.policyCacheMiddleware, service.changeContentMiddleware, service.runDetailMiddleware)
			service.registerNativeRoutes(service.echo)
			service.registerRunnerRoutes(service.echo)
			service.registerOperatorTools(service.echo)
		}
		if stage == "replay" {
			original := fixtures["org_first"]
			row := original.records["scheduled_validation_reports"][0]
			raw := strings.Replace(string(row.Values["record_json"].Data), strings.Repeat("a", 40), strings.Repeat("b", 40), 1)
			row.Values["record_json"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(raw)}
			changed := sources[0]
			changed.BundlePath = original.write()
			if count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, changed); err == nil || count != 0 {
				t.Fatalf("changed source count=%d error=%v", count, err)
			}
		}
		for _, source := range sources {
			count, err := ImportHealthScheduledReports(t.Context(), service.database.postgres, source)
			if err != nil || count != 6 {
				t.Fatalf("stage=%s count=%d error=%v", stage, count, err)
			}
			for _, hour := range []int{4, 11} {
				asOf := base.Add(time.Duration(hour) * time.Hour)
				want := slices.DeleteFunc(slices.Clone(expected[source.OrganizationID][source.ProjectID]), func(report healthstore.ScheduledReport) bool { return report.RecordedAt.After(asOf) })
				slices.SortFunc(want, func(a, b healthstore.ScheduledReport) int { return b.RecordedAt.Compare(a.RecordedAt) })
				if err := service.config.TenantRepository.ReadTenant(t.Context(), source.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
					history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), source.ProjectID, asOf)
					if !reflect.DeepEqual(history.Reports, want) || !history.FirstReport.Equal(persistedBase) || !history.LastGreen.Equal(persistedBase.Add(3*time.Hour)) {
						t.Fatalf("stage=%s org=%s project=%s history=%+v want=%+v error=%v", stage, source.OrganizationID, source.ProjectID, history, want, err)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				scheduled, err := service.readHealthScheduledRecords(t.Context(), source.OrganizationID, source.ProjectID, asOf)
				if err != nil || !scheduled.FirstReport.Equal(persistedBase) || !scheduled.LastReport.Equal(want[0].RecordedAt) || !scheduled.LastGreen.Equal(persistedBase.Add(3*time.Hour)) || scheduled.Cadence != time.Hour || scheduled.Failures != 1 {
					t.Fatalf("stage=%s evaluator history=%+v error=%v", stage, scheduled, err)
				}
			}
		}
	}
}
