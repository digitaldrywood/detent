package postgres_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthScheduledReportsKeepTenantRunReceipts(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}}})
				if err != nil {
					return err
				}
				record.Settings.CheckoutRepository = "example-org/example-project"
				_, err = w.UpdateProjectRecord(t.Context(), record)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for step, conclusion := range []string{"success", "failure"} {
				report := healthstore.ScheduledReport{OrganizationID: org, ProjectID: project, RecordedAt: now.Add(time.Duration(step) * time.Hour), ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: string(rune('1' + step)), RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/" + string(rune('1'+step)) + "/attempts/1", HeadSHA: strings.Repeat(string(rune('a'+index)), 40), Conclusion: conclusion}}
				write := func(report healthstore.ScheduledReport) error {
					return repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
						retained, err := w.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), report)
						if err == nil && !retained.RecordedAt.Equal(now.Add(time.Duration(step)*time.Hour)) {
							t.Fatalf("replay changed time: %+v", retained)
						}
						return err
					})
				}
				if err := write(report); err != nil {
					t.Fatal(err)
				}
				report.RecordedAt = now.Add(3 * time.Hour)
				if err := write(report); err != nil {
					t.Fatal(err)
				}
				report.Conclusion = "skipped"
				if err := write(report); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("changed replay=%v", err)
				}
				report.OrganizationID = "org_foreign"
				if err := write(report); !errors.Is(err, corestore.ErrTenantMismatch) {
					t.Fatalf("foreign write=%v", err)
				}
			}
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository, _ = pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
			for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
				history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), project, now.Add(2*time.Hour))
				if err != nil {
					return err
				}
				if len(history.Reports) != 2 || !history.FirstReport.Equal(now) || !history.LastGreen.Equal(now) {
					t.Fatalf("history=%+v", history)
				}
				if history.Reports[0].Conclusion != "failure" || history.Reports[0].HeadSHA != strings.Repeat(string(rune('a'+index)), 40) {
					t.Fatalf("foreign or reordered report=%+v", history.Reports)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			for step := range 27 {
				conclusion := "skipped"
				if step == 0 {
					conclusion = "failure"
				}
				run := strconv.Itoa(100 + step)
				at := now.Add(time.Duration(step+2) * time.Hour)
				if step == 26 {
					conclusion, at = "success", now.Add(72*time.Hour)
				}
				report := healthstore.ScheduledReport{OrganizationID: org, ProjectID: project, RecordedAt: at, ScheduledValidationReport: gate.ScheduledValidationReport{Repository: "example-org/example-project", RunID: run, RunAttempt: "1", RunURL: "https://github.com/example-org/example-project/actions/runs/" + run + "/attempts/1", HeadSHA: strings.Repeat(string(rune('a'+index)), 40), Conclusion: conclusion}}
				if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
					_, err := w.(healthstore.ScheduledWriter).RecordHealthScheduledReport(t.Context(), report)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
				history, err := r.(healthstore.ScheduledReader).HealthScheduledHistory(t.Context(), project, now.Add(40*time.Hour))
				if err != nil {
					return err
				}
				if len(history.Reports) != 23 || !history.FirstReport.Equal(now) || !history.LastGreen.Equal(now) || history.Reports[0].RunID != "125" {
					t.Fatalf("bounded history lost oldest/green/latest facts: %+v", history)
				}
				failures := map[string]bool{}
				for _, report := range history.Reports {
					if report.OrganizationID != org || report.ProjectID != project || report.HeadSHA != strings.Repeat(string(rune('a'+index)), 40) || report.RunID == "126" {
						t.Fatalf("bounded history changed scope/future cutoff: %+v", report)
					}
					if report.Conclusion == "failure" {
						failures[report.RunID] = true
					}
				}
				if len(failures) != 2 || !failures["2"] || !failures["100"] {
					t.Fatalf("older failures lost outside newest21: %+v", failures)
				}
				first, err := r.(healthstore.ScheduledReportReader).HealthScheduledReport(t.Context(), project, "example-org/example-project", "1", "1")
				if err == nil && (!first.RecordedAt.Equal(now) || first.Conclusion != "success") {
					t.Fatalf("point lookup lost immutable old run: %+v", first)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

}
