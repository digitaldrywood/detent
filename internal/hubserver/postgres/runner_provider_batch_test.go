package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type providerBatchProbe struct {
	pgquery.DBTX
	inserts    int
	statements int
	mutations  int64
	failTable  string
}

func (p *providerBatchProbe) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	p.statements++
	if strings.HasPrefix(query, "INSERT INTO runner_provider_") {
		p.inserts++
		if p.failTable != "" && strings.HasPrefix(query, "INSERT INTO "+p.failTable+" ") {
			return p.DBTX.ExecContext(ctx, "SELECT 1/0")
		}
	}
	result, err := p.DBTX.ExecContext(ctx, query, args...)
	if err == nil && (strings.HasPrefix(query, "INSERT INTO runner_") || strings.HasPrefix(query, "DELETE FROM runner_")) {
		n, _ := result.RowsAffected()
		p.mutations += n
	}
	return result, err
}

func (p *providerBatchProbe) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	p.statements++
	return p.DBTX.QueryContext(ctx, query, args...)
}
func (p *providerBatchProbe) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	p.statements++
	return p.DBTX.QueryRowContext(ctx, query, args...)
}

func TestRunnerProviderReportsBatchStatementsAndFidelity(t *testing.T) {
	fixture := pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
		roles, err := os.ReadFile("roles.sql")
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, string(roles)); err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, "SET ROLE detent_migration_v2"); err != nil {
			return err
		}
		provider, err := goose.NewProvider(goose.DialectPostgres, db, RoleMigrations(os.DirFS("migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
		if err != nil {
			return err
		}
		_, err = provider.Up(ctx)
		return err
	})
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	admin := fixture.Database(t)
	name, password := "example_"+strings.ToLower(rand.Text()), rand.Text()
	quoted := pq.QuoteIdentifier(name)
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pq.QuoteLiteral(password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP OWNED BY "+quoted); err != nil {
			t.Error(err)
		}
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.ExecContext(t.Context(), "GRANT detent_runtime_v2 TO "+quoted); err != nil {
		t.Fatal(err)
	}
	pool := fixture.OpenRole(t, admin, name, password)
	repository, err := NewRepository(t.Context(), pool, PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
		for _, runner := range []string{"runner_first", "runner_second"} {
			seedBatchRunnerIdentity(t, repository, org, runner, "machine_"+runner, now, true)
		}
	}
	expected := make(map[string][]providercapacity.Report)
	write := func(t *testing.T, org tracker.OrganizationID, runner string, reports []providercapacity.Report, failTable string, rollback bool) {
		t.Helper()
		sentinel := errors.New("rollback requested")
		err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
			probe := &providerBatchProbe{DBTX: s.db, failTable: failTable}
			s.db = probe
			if err := s.ReplaceRunnerProviderReports(t.Context(), runner, reports); err != nil {
				return err
			}
			models, efforts, windows := 0, 0, 0
			for _, report := range reports {
				models += len(report.Models)
				windows += len(report.UsageWindows)
				for _, detail := range report.ModelDetails {
					efforts += len(detail.ReasoningEfforts)
				}
			}
			bound := (len(reports)+499)/500 + (models+499)/500 + (efforts+499)/500 + (windows+499)/500
			t.Logf("reports=%d models=%d efforts=%d windows=%d inserts=%d bound=%d", len(reports), models, efforts, windows, probe.inserts, bound)
			if probe.inserts != bound {
				t.Errorf("insert statements=%d, want batch bound=%d", probe.inserts, bound)
			}
			if rollback {
				return sentinel
			}
			return nil
		})
		if failTable != "" {
			var pgError *pq.Error
			if !errors.As(err, &pgError) || pgError.Code != "22012" {
				t.Fatalf("injected PostgreSQL division error: %v", err)
			}
		} else if rollback {
			if !errors.Is(err, sentinel) {
				t.Fatalf("rollback: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			expected[string(org)+runner] = reports
		}
	}
	check := func(t *testing.T) {
		t.Helper()
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, runner := range []string{"runner_first", "runner_second"} {
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					got, err := r.RunnerProviderReports(t.Context(), runner)
					if err != nil {
						return err
					}
					want := expected[string(org)+runner]
					if !reflect.DeepEqual(got, want) {
						t.Errorf("%s/%s report fidelity mismatch: got=%+v want=%+v", org, runner, got, want)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, runner := range []string{"runner_first", "runner_second"} {
			reports := batchProviderReports(now, 2, 3)
			for i := range reports {
				reports[i].AccountAlias = fmt.Sprintf("%s_%s_%d", org, runner, i)
			}
			write(t, org, runner, reports, "", false)
		}
	}
	check(t)
	t.Run("unchanged_report_does_not_mutate", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
				probe := &providerBatchProbe{DBTX: s.db}
				s.db = probe
				started := time.Now()
				if err := s.ReplaceRunnerProviderReports(t.Context(), "runner_first", expected[string(org)+"runner_first"]); err != nil {
					return err
				}
				t.Logf("org=%s statements=%d mutations=%d hold=%s", org, probe.statements, probe.mutations, time.Since(started))
				if probe.mutations != 0 {
					t.Errorf("unchanged report mutations=%d, want 0", probe.mutations)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
	for _, tc := range []struct {
		name            string
		reports, models int
	}{{"small", 1, 1}, {"models_and_efforts", 2, 13}, {"cross_500_rows", 5, 101}} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, "org_first", "runner_first", batchProviderReports(now, tc.reports, tc.models), "", false)
			check(t)
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
					report := batchProviderReports(now, tc.reports, tc.models)
					if err := s.ReplaceRunnerProviderReports(t.Context(), "runner_second", report); err != nil {
						return err
					}
					probe := &providerBatchProbe{DBTX: s.db}
					s.db = probe
					started := time.Now()
					if err := s.ReplaceRunnerProviderReports(t.Context(), "runner_second", report); err != nil {
						return err
					}
					t.Logf("provider org=%s reports=%d models=%d statements=%d mutations=%d hold=%s", org, tc.reports, tc.models, probe.statements, probe.mutations, time.Since(started))
					if probe.mutations != 0 || probe.statements > 10 {
						t.Errorf("unchanged provider statements=%d mutations=%d", probe.statements, probe.mutations)
					}
					return errors.New("discard replay fixture")
				}); err == nil || err.Error() != "discard replay fixture" {
					t.Fatalf("replay rollback: %v", err)
				}
			}
		})
	}
	for _, tc := range []struct {
		name          string
		count, models int
	}{{"observed_1_3_0_1", 1, 3}, {"observed_2_13_0_1", 2, 13}} {
		t.Run(tc.name, func(t *testing.T) {
			reports := batchProviderReports(now, tc.count, tc.models)
			for i := range reports {
				reports[i].ModelDetails = []providercapacity.ModelDetail{}
				reports[i].Models = reports[i].Models[:tc.models/tc.count]
				reports[i].UsageWindows = []providercapacity.UsageWindow{}
			}
			if tc.count == 2 {
				reports[1].Models = append(reports[1].Models, "model_extra")
			}
			reports[0].UsageWindows = []providercapacity.UsageWindow{{Kind: "weekly", ObservedAt: now}}
			write(t, "org_first", "runner_first", reports, "", false)
			check(t)
		})
	}
	t.Run("callback_rollback", func(t *testing.T) {
		write(t, "org_first", "runner_first", batchProviderReports(now, 1, 2), "", true)
		check(t)
	})
	for _, table := range []string{"runner_provider_report_models", "runner_provider_model_efforts", "runner_provider_usage_windows"} {
		t.Run("rollback_"+table, func(t *testing.T) {
			write(t, "org_first", "runner_first", batchProviderReports(now, 2, 4), table, false)
			check(t)
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*providercapacity.Report)
	}{
		{"parent", func(r *providercapacity.Report) { r.MaxConcurrent++ }},
		{"model", func(r *providercapacity.Report) { r.ModelDetails[0].Label = "Changed" }},
		{"effort", func(r *providercapacity.Report) { r.ModelDetails[0].ReasoningEfforts[0] = "extra" }},
		{"window", func(r *providercapacity.Report) { r.UsageWindows[1].PercentUsed = new(90.0) }},
		{"observation", func(r *providercapacity.Report) { r.ObservedAt = r.ObservedAt.Add(time.Second) }},
	} {
		t.Run("changed_"+tc.name, func(t *testing.T) {
			reports := batchProviderReports(now, 2, 3)
			if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerProviderReports(t.Context(), "runner_first", reports)
			}); err != nil {
				t.Fatal(err)
			}
			tc.change(&reports[0])
			write(t, "org_first", "runner_first", reports, "", false)
			check(t)
		})
	}
	t.Run("skill_replay_and_freshness", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_report", Name: "Example", Slug: "report", Profile: "native", WorkflowSource: "example", WorkflowSourceRevision: "source", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{1, 2000} {
				report := make([]skills.ProviderSkill, size)
				for i := range report {
					report[i] = skills.ProviderSkill{Name: fmt.Sprintf("skill_%d", i), Path: fmt.Sprintf("/project/skill_%d/SKILL.md", i), Scope: "project", Enabled: true, UserInvocable: true, Invocation: fmt.Sprintf("$skill_%d", i)}
				}
				for _, step := range []struct {
					name      string
					at        time.Time
					unchanged bool
				}{{"initial", now, false}, {"replay", now, true}, {"fresh", now.Add(time.Second), false}, {"content", now.Add(time.Second), false}} {
					if step.name == "content" {
						report[0].Description = "Changed"
					}
					if err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
						probe := &providerBatchProbe{DBTX: s.db}
						s.db = probe
						started := time.Now()
						if err := s.ReplaceRunnerProjectSkills(t.Context(), "runner_first", "prj_report", report, step.at); err != nil {
							return err
						}
						t.Logf("skills org=%s size=%d step=%s statements=%d mutations=%d hold=%s", org, size, step.name, probe.statements, probe.mutations, time.Since(started))
						if step.unchanged && (probe.mutations != 0 || probe.statements > 7) {
							t.Errorf("unchanged skills statements=%d mutations=%d", probe.statements, probe.mutations)
						}
						got, err := s.RunnerProjectSkills(t.Context(), "prj_report", "runner_first", step.at, runnerstore.MaxProjectSkills)
						if err != nil {
							return err
						}
						if len(got) != size || got[0].Skill != report[0] || !got[0].ReportedAt.Equal(step.at) {
							t.Errorf("skill content/freshness mismatch: count=%d", len(got))
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	})
	t.Run("empty_replacement", func(t *testing.T) {
		write(t, "org_first", "runner_first", []providercapacity.Report{}, "", false)
		check(t)
	})
}

func batchProviderReports(now time.Time, count, models int) []providercapacity.Report {
	reports := make([]providercapacity.Report, 0, count)
	for i := 0; i < count; i++ {
		report := providercapacity.Report{Backend: fmt.Sprintf("backend_%02d", i), Provider: "example", AccountAlias: fmt.Sprintf("account_%d", i), SharedAccountAlias: "shared_example", Billing: "subscription", MaxConcurrent: i + 1, Availability: "available", ObservedAt: now, Models: []string{}, ModelDetails: []providercapacity.ModelDetail{}, UsageWindows: []providercapacity.UsageWindow{}}
		if i%2 == 0 {
			report.ResetAt = now.Add(time.Hour)
		}
		for j := 0; j < models; j++ {
			id := fmt.Sprintf("model_%03d", models-j)
			report.Models = append(report.Models, id)
			if j%3 != 1 {
				detail := providercapacity.ModelDetail{ID: id, Label: "Example label " + id, Provider: "example_model_provider", Default: j == 0, Legacy: j%2 == 0}
				if j%3 == 0 {
					detail.ReasoningEfforts = []string{"high", "low", "medium", "max"}
					detail.DefaultReasoningEffort = "low"
				}
				report.ModelDetails = append(report.ModelDetails, detail)
			}
		}
		percent := 12.5
		report.UsageWindows = []providercapacity.UsageWindow{{Kind: "weekly"}, {Kind: "primary", PercentUsed: &percent, ResetAt: now.Add(2 * time.Hour), ObservedAt: now}}
		reports = append(reports, report)
	}
	return reports
}

func seedBatchRunnerIdentity(t *testing.T, repository *Repository, organization tracker.OrganizationID, runnerID, machineID string, now time.Time, registerMachine bool) {
	t.Helper()
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if registerMachine {
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: runnerID, Name: runnerID, Hash: apikey.HashToken("credential_" + string(organization) + "_" + runnerID), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: new(now.Add(time.Hour)), ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		enrollment := runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + runnerID, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("enrollment_" + string(organization) + "_" + runnerID), Scope: "organization", Operations: []string{"read", "claim"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), CreatedBy: "admin"}
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, runnerID, tracker.MachineID(machineID), now); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: runnerID, EnrollmentID: enrollment.ID, Scope: "organization", Operations: enrollment.Operations, CapacityLimit: 1, ReportedCapacity: 1, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastHeartbeatAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
}
