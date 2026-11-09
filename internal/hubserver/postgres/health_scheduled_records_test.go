package postgres_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHealthScheduledReportsKeepTenantRunReceipts(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	now := historyAt()
	for index, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, org)
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
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
	repository, _ = repositoryFor(t, admin, smallPool())
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
}
