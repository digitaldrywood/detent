package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/pgtransport"

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

type providerWireCounts struct {
	statements atomic.Int64
	exchanges  atomic.Int64
}

type providerWireConn struct {
	trace pgtest.FrontendTrace
	net.Conn
	counts  *providerWireCounts
	pending []byte
	startup bool
}

func (c *providerWireConn) Write(value []byte) (int, error) {
	n, err := c.Conn.Write(value)
	c.pending = append(c.pending, value[:n]...)
	for {
		offset := 1
		if c.startup {
			offset = 0
		}
		if len(c.pending) < offset+4 {
			break
		}
		size := offset + int(binary.BigEndian.Uint32(c.pending[offset:offset+4]))
		if len(c.pending) < size {
			break
		}
		if c.startup {
			c.startup = false
		} else {
			cost := c.trace.Count(c.pending[:size])
			c.counts.statements.Add(int64(cost.Statements))
			c.counts.exchanges.Add(int64(cost.ExecutionExchanges))
		}
		c.pending = c.pending[size:]
	}
	return n, err
}

func (p *providerBatchProbe) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	p.statements++
	if strings.HasPrefix(query, "INSERT INTO runner_provider_") {
		p.inserts++
		if p.failTable != "" && strings.HasPrefix(query, "INSERT INTO "+p.failTable+" ") {
			return p.DBTX.Exec(ctx, "SELECT 1/0")
		}
	}
	result, err := p.DBTX.Exec(ctx, query, args...)
	if err == nil && (strings.HasPrefix(query, "INSERT INTO runner_") || strings.HasPrefix(query, "DELETE FROM runner_")) {
		n := result.RowsAffected()
		p.mutations += n
	}
	return result, err
}

func (p *providerBatchProbe) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	p.statements++
	return p.DBTX.Query(ctx, query, args...)
}
func (p *providerBatchProbe) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	p.statements++
	return p.DBTX.QueryRow(ctx, query, args...)
}

func TestRunnerProviderReportsBatchStatementsAndFidelity(t *testing.T) {
	t.Parallel()
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
		provider, err := goose.NewProvider(goose.DialectPostgres, db, RoleMigrations(os.DirFS("migrations")), goose.WithGoMigrations(ConversationMigrations(os.DirFS("migrations"))...), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
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
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)); err != nil {
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
	native := fixture.NativeConfig(t, admin, name, password)
	native.MaxConns = 3
	counts := &providerWireCounts{}
	dial := native.ConnConfig.DialFunc
	native.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &providerWireConn{Conn: conn, counts: counts, startup: true}, nil
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
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
			var pgError *pgconn.PgError
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
	for _, billing := range []string{"", "unknown", "subscription", "api_key"} {
		t.Run("billing_"+billing, func(t *testing.T) {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(string(org), func(t *testing.T) {
					reports := batchProviderReports(now, 1, 2)
					reports[0].Billing = billing
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.ReplaceRunnerProviderReports(t.Context(), "runner_first", reports)
					}); err != nil {
						t.Fatal(err)
					}
					if reports[0].Billing != billing {
						t.Fatal("writer changed caller's report")
					}
					reports[0].Billing = reports[0].BillingBasis()
					expected[string(org)+"runner_first"] = reports
					check(t)
				})
			}
		})
	}
	t.Run("omitted_billing_fixed_wire_cost", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			var firstStatements, firstExchanges int64
			for _, size := range []int{2, 50} {
				reports := batchProviderReports(now, 1, size)
				reports[0].Billing = ""
				reports[0].ObservedAt = now.Add(time.Duration(size) * time.Second)
				counts.statements.Store(0)
				counts.exchanges.Store(0)
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerProviderReports(t.Context(), "runner_first", reports)
				}); err != nil {
					t.Fatal(err)
				}
				statements, exchanges := counts.statements.Load(), counts.exchanges.Load()
				t.Logf("org=%s models=%d statements=%d exchanges=%d", org, size, statements, exchanges)
				if statements != 13 || exchanges != 12 || size == 50 && (statements != firstStatements || exchanges != firstExchanges) {
					t.Fatalf("provider wire cost grew: %d/%d versus %d/%d", statements, exchanges, firstStatements, firstExchanges)
				}
				firstStatements, firstExchanges = statements, exchanges
				counts.statements.Store(0)
				counts.exchanges.Store(0)
				if err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
					probe := &providerBatchProbe{DBTX: s.db}
					s.db = probe
					if err := s.ReplaceRunnerProviderReports(t.Context(), "runner_first", reports); err != nil {
						return err
					}
					if probe.mutations != 0 {
						t.Fatalf("omitted billing replay mutated %d rows", probe.mutations)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				t.Logf("org=%s models=%d replay statements=%d exchanges=%d", org, size, counts.statements.Load(), counts.exchanges.Load())
				if counts.statements.Load() != 9 || counts.exchanges.Load() != 8 {
					t.Fatalf("replay wire cost: %d/%d", counts.statements.Load(), counts.exchanges.Load())
				}
				reports[0].Billing = "unknown"
				expected[string(org)+"runner_first"] = reports
				check(t)
			}
		}
	})
	for _, tc := range []struct {
		name   string
		change func(*providercapacity.Report)
	}{
		{"billing", func(r *providercapacity.Report) { r.Billing = "invalid" }},
		{"availability", func(r *providercapacity.Report) { r.Availability = "" }},
		{"usage_kind", func(r *providercapacity.Report) { r.UsageWindows[0].Kind = "" }},
		{"default_effort", func(r *providercapacity.Report) { r.ModelDetails[0].DefaultReasoningEffort = "unsupported" }},
	} {
		t.Run("reject_invalid_"+tc.name, func(t *testing.T) {
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				reports := batchProviderReports(now, 1, 2)
				tc.change(&reports[0])
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					return w.ReplaceRunnerProviderReports(t.Context(), "runner_first", reports)
				})
				if !errors.Is(err, runnerstore.ErrInvalidRecord) {
					t.Fatalf("invalid report accepted: %v", err)
				}
			}
			check(t)
		})
	}
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
						got, truncated, err := s.RunnerProjectSkills(t.Context(), "prj_report", "runner_first", step.at, runnerstore.MaxProjectSkills)
						if err != nil {
							return err
						}
						if len(got) != size || truncated || got[0].Skill != report[0] || !got[0].ReportedAt.Equal(step.at) {
							t.Errorf("skill content/freshness mismatch: count=%d truncated=%t", len(got), truncated)
						}
						if size > 1 {
							capped, truncated, err := s.RunnerProjectSkills(t.Context(), "prj_report", "runner_first", step.at, size-1)
							if err != nil {
								return err
							}
							if len(capped) != size-1 || !truncated || capped[0].Skill != report[0] {
								t.Errorf("capped skill read count=%d truncated=%t, want %d true", len(capped), truncated, size-1)
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	})
	t.Run("catalog_scope_freshness_and_current_authority", func(t *testing.T) {
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			firstID, secondID, otherID := string(org)+"/z-model", string(org)+"/a-model", string(org)+"/other-model"
			firstReport := providercapacity.Report{Backend: "example", Provider: "example", AccountAlias: "example", Billing: "subscription", MaxConcurrent: 1, Availability: "exhausted", ObservedAt: now, UsageWindows: []providercapacity.UsageWindow{}, Models: []string{firstID, secondID}, ModelDetails: []providercapacity.ModelDetail{{ID: firstID, Label: "Example model", Provider: "model-provider", ReasoningEfforts: []string{"medium", "max"}, DefaultReasoningEffort: "max", Default: true, Legacy: true}}}
			otherReport := firstReport
			otherReport.Models, otherReport.ModelDetails = []string{otherID}, []providercapacity.ModelDetail{}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_catalog_other", Name: "Other project", Slug: "catalog-other", Profile: "native", WorkflowSource: "example", WorkflowSourceRevision: "source", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")}); err != nil {
					return err
				}
				for _, row := range []struct {
					runner  string
					project tracker.ProjectID
					report  providercapacity.Report
				}{{"runner_first", "prj_report", firstReport}, {"runner_second", "prj_catalog_other", otherReport}} {
					identity, err := w.RunnerIdentity(t.Context(), row.runner)
					if err != nil {
						return err
					}
					identity.Scope = "projects"
					if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: row.runner, ProjectID: row.project}); err != nil {
						return err
					}
					if err := w.ReplaceRunnerProviderReports(t.Context(), row.runner, []providercapacity.Report{row.report}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			expected[string(org)+"runner_first"] = []providercapacity.Report{firstReport}
			expected[string(org)+"runner_second"] = []providercapacity.Report{otherReport}
			for _, tc := range []struct {
				name     string
				projects []tracker.ProjectID
				want     []string
				change   func(*session) error
			}{
				{name: "all_tenant_projects", want: []string{firstID, secondID, otherID}},
				{name: "empty_projects", projects: []tracker.ProjectID{}, want: []string{}},
				{name: "selected_project", projects: []tracker.ProjectID{"prj_report"}, want: []string{firstID, secondID}},
				{name: "other_project", projects: []tracker.ProjectID{"prj_catalog_other"}, want: []string{otherID}},
				{name: "project_union", projects: []tracker.ProjectID{"prj_catalog_other", "prj_report"}, want: []string{firstID, secondID, otherID}},
				{name: "unknown_project", projects: []tracker.ProjectID{"prj_missing"}, want: []string{}},
				{name: "duplicate_model_first_report_wins", want: []string{firstID, secondID}, change: func(s *session) error {
					report := otherReport
					report.Models = []string{firstID}
					return s.ReplaceRunnerProviderReports(t.Context(), "runner_second", []providercapacity.Report{report})
				}},
				{name: "organization_runner", projects: []tracker.ProjectID{"prj_report"}, want: []string{firstID, secondID, otherID}, change: func(s *session) error {
					identity, err := s.RunnerIdentity(t.Context(), "runner_second")
					if err != nil {
						return err
					}
					identity.Scope = "organization"
					return s.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				}},
				{name: "withdrawn_runner_grant", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					return s.DeleteAccessTokenGrant(t.Context(), "runner_first", "prj_report")
				}},
				{name: "expired_runner", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					identity, err := s.RunnerIdentity(t.Context(), "runner_first")
					if err != nil {
						return err
					}
					identity.ExpiresAt = now
					return s.UpdateRunnerIdentity(t.Context(), identity)
				}},
				{name: "inactive_runner", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					identity, err := s.RunnerIdentity(t.Context(), "runner_first")
					if err != nil {
						return err
					}
					identity.State = "draining"
					return s.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				}},
				{name: "removed_runner", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					return s.RemoveRunnerIdentity(t.Context(), "runner_first", now)
				}},
				{name: "revoked_credential", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					return s.RevokeAccessCredential(t.Context(), "runner_first", now)
				}},
				{name: "expired_credential", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					return s.RenewAccessCredential(t.Context(), "runner_first", apikey.HashToken("renewed/"+string(org)), "example", now, now)
				}},
				{name: "deleted_project", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					project, err := s.Project(t.Context(), "prj_report")
					if err != nil {
						return err
					}
					project.DeletedAt = &now
					_, err = s.UpdateProject(t.Context(), project)
					return err
				}},
				{name: "exact_stale_boundary", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					report := firstReport
					report.ObservedAt = now.Add(-providercapacity.MaxAge)
					return s.ReplaceRunnerProviderReports(t.Context(), "runner_first", []providercapacity.Report{report})
				}},
				{name: "just_fresh_boundary", projects: []tracker.ProjectID{"prj_report"}, want: []string{firstID, secondID}, change: func(s *session) error {
					report := firstReport
					report.ObservedAt = now.Add(-providercapacity.MaxAge + time.Microsecond)
					return s.ReplaceRunnerProviderReports(t.Context(), "runner_first", []providercapacity.Report{report})
				}},
				{name: "future_report", projects: []tracker.ProjectID{"prj_report"}, want: []string{}, change: func(s *session) error {
					report := firstReport
					report.ObservedAt = now.Add(time.Microsecond)
					return s.ReplaceRunnerProviderReports(t.Context(), "runner_first", []providercapacity.Report{report})
				}},
				{name: "expired_capacity_reset_is_not_catalog_authority", projects: []tracker.ProjectID{"prj_report"}, want: []string{firstID, secondID}, change: func(s *session) error {
					report := firstReport
					report.ObservedAt, report.ResetAt = now.Add(-time.Second), now.Add(-time.Microsecond)
					return s.ReplaceRunnerProviderReports(t.Context(), "runner_first", []providercapacity.Report{report})
				}},
			} {
				t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
					rollback := errors.New("catalog test rollback")
					err := repository.transaction(t.Context(), org, corestore.Serving, false, func(s *session) error {
						if tc.change != nil {
							if err := tc.change(s); err != nil {
								return err
							}
						}
						probe := &providerBatchProbe{DBTX: s.db}
						s.db = probe
						catalog, err := s.RunnerProviderCatalog(t.Context(), tc.projects, now)
						if err != nil {
							return err
						}
						ids := make([]string, 0, len(catalog))
						for _, model := range catalog {
							ids = append(ids, model.ID)
							if model.ID == firstID && (model.Detail == nil || model.Detail.Label != "Example model" || model.Detail.Provider != "model-provider" || !model.Detail.Default || !model.Detail.Legacy || model.Detail.DefaultReasoningEffort != "max" || !reflect.DeepEqual(model.Detail.ReasoningEfforts, []string{"medium", "max"})) {
								t.Errorf("catalog detail = %+v", model)
							}
							if model.ID != firstID && model.Detail != nil {
								t.Errorf("identifier-only model acquired detail: %+v", model)
							}
						}
						if !reflect.DeepEqual(ids, tc.want) {
							t.Errorf("catalog model IDs = %v, want %v", ids, tc.want)
						}
						wantStatements := 1
						if tc.projects != nil && len(tc.projects) == 0 {
							wantStatements = 0
						}
						if probe.statements != wantStatements {
							t.Errorf("catalog statements = %d, want %d", probe.statements, wantStatements)
						}
						return rollback
					})
					if !errors.Is(err, rollback) {
						t.Fatal(err)
					}
				})
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
