package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerHeartbeatReportsAvoidRowAmplification(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	now := historyAt()
	counter := new(reportCountingTracer)
	counted, err := postgres.NewRepository(t.Context(), tracedFeaturePool(t, pool, counter), smallPool())
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return seedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		seedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) VALUES($1,'project_shared','Example','native')`, org); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{2, 50} {
			problems := make([]runnerauth.Problem, size)
			states := make([]string, size)
			for i := range size {
				problems[i] = runnerauth.Problem{Code: "backend_missing", Subject: fmt.Sprint(i), Message: "Example", FixHint: "Retry", ReportedAt: now, FirstSeen: now}
				states[i] = fmt.Sprintf("State %d", i)
			}
			for _, kind := range []string{"problems", "admission"} {
				table := "runner_problem_reports"
				if kind == "admission" {
					table = "runner_native_admission_selectors"
				}
				var before string
				for _, repeat := range []bool{false, true} {
					err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
						counter.count.Store(0)
						var err error
						if kind == "problems" {
							err = w.ReplaceRunnerProblems(ctx, "runner_shared", problems)
						} else {
							err = w.ReplaceRunnerAdmissionObservation(ctx, "runner_shared", "project_shared", tracker.NativeAdmissionObservation{RunnerRevision: 1, ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: "example", LastRefreshAt: now, RefreshDuration: time.Second, WorkflowStates: states[:size/2], Authors: states[size/2:]}})
						}
						statements := counter.count.Load()
						t.Logf("tenant=%s report=%s rows=%d repeated=%t statements=%d", org, kind, size, repeat, statements)
						if statements > 4 {
							t.Errorf("%s writes scale with report rows: %d", kind, statements)
						}
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
					var count int
					var versions string
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*),COALESCE(string_agg(xmin::text,',' ORDER BY ctid),'') FROM `+table+` WHERE organization_id=$1 AND runner_id='runner_shared'`, org).Scan(&count, &versions); err != nil {
						t.Fatal(err)
					}
					if count != size {
						t.Errorf("%s rows=%d, want %d", kind, count, size)
					}
					if repeat && versions != before {
						t.Errorf("unchanged %s rewrote report rows", kind)
					}
					before = versions
				}
			}
		}
	}
}
