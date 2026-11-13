package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestRunnerHeartbeatReportsAvoidRowAmplification(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	counter := new(pgstoretest.ReportCountingTracer)
	counted, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, counter), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) VALUES($1,'project_shared','Example','native')`, org); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{2, 50} {
			problems := make([]runnerauth.Problem, size)
			states := make([]string, size)
			for i := range size {
				problems[i] = runnerauth.Problem{Code: "backend_missing", Subject: fmt.Sprint(i), Message: "Example", FixHint: "Retry", ReportedAt: now, FirstSeen: now, CachedCapabilities: i%2 == 0}
				states[i] = fmt.Sprintf("State %d", i)
			}
			assertProblems := func(actual []runnerauth.Problem) {
				t.Helper()
				if len(actual) != len(problems) {
					t.Fatalf("problem roundtrip count = %d, want %d", len(actual), len(problems))
				}
				expected := make(map[string]runnerauth.Problem, len(problems))
				for _, problem := range problems {
					expected[problem.Subject] = problem
				}
				for _, problem := range actual {
					want, ok := expected[problem.Subject]
					problem.ReportedAt, problem.FirstSeen = problem.ReportedAt.UTC(), problem.FirstSeen.UTC()
					want.ReportedAt, want.FirstSeen = want.ReportedAt.UTC(), want.FirstSeen.UTC()
					if !ok || !reflect.DeepEqual(problem, want) {
						t.Fatalf("problem discriminator or details changed: actual=%+v want=%+v", problem, want)
					}
					delete(expected, problem.Subject)
				}
			}
			checkProblemReaders := func() {
				t.Helper()
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
					problems, err := reader.RunnerProblems(t.Context(), "runner_shared")
					if err != nil {
						return err
					}
					assertProblems(problems)
					record, err := reader.(runnerstore.RoutingReader).RunnerRoutingRecord(t.Context(), "runner_shared", now)
					if err != nil {
						return err
					}
					if record.Record.Identity.OrganizationID != org || record.Record.Identity.RunnerID != "runner_shared" {
						t.Fatalf("problem routing owner escaped scope: %+v", record.Record.Identity)
					}
					assertProblems(record.Record.Problems)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					problems, err := w.RunnerProblems(ctx, "runner_shared")
					if err != nil {
						return err
					}
					assertProblems(problems)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []string{"problems", "admission", "backend", "workspace", "checkout"} {
				table := "runner_problem_reports"
				if kind == "admission" {
					table = "runner_native_admission_selectors"
				}
				wantCount := size
				if kind == "backend" {
					table = "runner_backend_isolations"
				}
				if kind == "workspace" {
					table = "runner_workspace_reports"
					wantCount = 1
				}
				if kind == "checkout" {
					table = "runner_checkout_repositories"
					wantCount = 1
				}
				var before string
				for _, repeat := range []bool{false, true} {
					err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
						counter.Count.Store(0)
						var err error
						switch kind {
						case "problems":
							err = w.ReplaceRunnerProblems(ctx, "runner_shared", problems)
						case "backend":
							report := isolation.Report{}
							for i := range size {
								report[fmt.Sprintf("example_%d", i)] = []string{isolation.NativeTrusted}
							}
							err = w.ReplaceRunnerBackendIsolation(ctx, "runner_shared", report)
						case "checkout":
							err = w.ReplaceRunnerCheckoutReport(ctx, runnerstore.CheckoutReport{OrganizationID: org, RunnerID: "runner_shared", ProjectID: "project_shared", Repository: "example-org/" + string(org), ReportedAt: now})
						case "workspace":
							err = w.ReplaceRunnerWorkspaceReport(ctx, "runner_shared", workspacesession.Capabilities{Terminal: true, Files: true, Git: true}, "sandbox")
						default:
							err = w.ReplaceRunnerAdmissionObservation(ctx, "runner_shared", "project_shared", tracker.NativeAdmissionObservation{RunnerRevision: 1, ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: "example", LastRefreshAt: now, RefreshDuration: time.Second, WorkflowStates: states[:size/2], Authors: states[size/2:]}})
						}
						statements := counter.Count.Load()
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
					if count != wantCount {
						t.Errorf("%s rows=%d, want %d", kind, count, wantCount)
					}
					if repeat && versions != before {
						t.Errorf("unchanged %s rewrote report rows", kind)
					}
					if kind == "checkout" {
						if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
							row, err := reader.RunnerCheckoutReport(t.Context(), "runner_shared", "project_shared")
							if err != nil {
								return err
							}
							if row.OrganizationID != org || row.RunnerID != "runner_shared" || row.ProjectID != "project_shared" || row.Repository != "example-org/"+string(org) || !row.ReportedAt.Equal(now) {
								t.Fatalf("selected checkout escaped tenant/runner/project: %+v", row)
							}
							for _, missing := range []struct {
								runner  string
								project tracker.ProjectID
							}{{"other_runner", "project_shared"}, {"runner_shared", "other_project"}} {
								if _, err := reader.RunnerCheckoutReport(t.Context(), missing.runner, missing.project); !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("selected checkout escaped exact scope: %+v error=%v", missing, err)
								}
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "problems" {
						checkProblemReaders()
					}
					before = versions
				}
				if kind == "problems" {
					problems[0].CachedCapabilities = !problems[0].CachedCapabilities
					if err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
						return w.ReplaceRunnerProblems(ctx, "runner_shared", problems)
					}); err != nil {
						t.Fatal(err)
					}
					checkProblemReaders()
				}
			}
		}
	}
}

func TestRunnerProjectObservationPreservesUnchangedDetails(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO projects(organization_id,id,name,profile) VALUES($1,'project_shared','Example','native')`, org); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{2, 50} {
			row := runnerstore.ProjectObservation{OrganizationID: org, RunnerID: "runner_shared", ProjectID: "project_shared", ObservedAt: now.Add(time.Duration(size) * time.Minute), SnapshotDigest: strings.Repeat(fmt.Sprint(size%10), 64)}
			attempts := make([]runnerstore.ProjectDiagnosticAttempt, size)
			for i := range attempts {
				attempts[i] = runnerstore.ProjectDiagnosticAttempt{Position: i, RecordKey: fmt.Sprint(i), IssueID: "wi_example", LocalAttemptID: int64(i + 1), Membership: []string{"running", `quoted,"value`}}
			}
			var before string
			for _, repeat := range []bool{false, true} {
				row.ObservedAt = row.ObservedAt.Add(time.Second)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.ReplaceRunnerProjectObservation(t.Context(), row, attempts) }); err != nil {
					t.Fatal(err)
				}
				var versions string
				var count int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*),string_agg(xmin::text,',' ORDER BY position) FROM runner_project_diagnostic_attempts WHERE organization_id=$1 AND runner_id='runner_shared' AND project_id='project_shared'`, org).Scan(&count, &versions); err != nil {
					t.Fatal(err)
				}
				if count != size {
					t.Errorf("diagnostics count=%d, want %d", count, size)
				}
				if repeat && versions != before {
					t.Errorf("unchanged project diagnostics rewrote %d rows", size)
				}
				before = versions
			}
		}
	}
}

func TestRunnerHeartbeatHostSegmentsHaveFixedStatements(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	now := pgstoretest.HistoryAt()
	counter := new(pgstoretest.ReportCountingTracer)
	counted, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, counter), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return pgstoretest.SeedCoreOrganization(t.Context(), w, org) }); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_shared", now, true)
		for _, size := range []int{2, 24} {
			segments := make([]hostmetrics.Summary, size)
			for i := range segments {
				segments[i] = hostmetrics.Summary{Hour: now.Truncate(time.Hour), SegmentID: now.Add(-time.Duration(i+1) * time.Second), SampleCount: 1, LogicalCores: 2}
			}
			for _, repeat := range []bool{false, true} {
				if err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
					counter.Count.Store(0)
					acknowledged, err := w.AppendRunnerHostMetricSegments(ctx, "runner_shared", segments, now)
					if err != nil {
						return err
					}
					if len(acknowledged) != size {
						t.Errorf("acknowledgments=%d, want %d", len(acknowledged), size)
					}
					statements := counter.Count.Load()
					t.Logf("tenant=%s segments=%d repeated=%t statements=%d", org, size, repeat, statements)
					if statements > 3 {
						t.Errorf("host segment statements grow per row: %d", statements)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id='runner_shared'`, org).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != size {
					t.Errorf("host segment rows=%d, want %d", count, size)
				}
			}
		}
		for offset := 24; offset < 144; offset += 24 {
			segments := make([]hostmetrics.Summary, 24)
			for i := range segments {
				segments[i] = hostmetrics.Summary{Hour: now.Truncate(time.Hour), SegmentID: now.Add(-time.Duration(offset+i+1) * time.Second), SampleCount: 1, LogicalCores: 2}
			}
			if err := counted.WriteRunnerHeartbeat(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
				acknowledged, err := w.AppendRunnerHostMetricSegments(ctx, "runner_shared", segments, now)
				if err == nil && len(acknowledged) != 24 {
					t.Errorf("capped acknowledgments=%d", len(acknowledged))
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_host_metric_segments WHERE organization_id=$1 AND runner_id='runner_shared'`, org).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 128 {
			t.Errorf("hour cap=%d, want 128", count)
		}
	}
}
