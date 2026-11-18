package hubserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestHostedAnalyticsReads(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.Clock = func() time.Time { return now }
		cfg.RunDetails = &memoryRunDetails{}
	})
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "In Progress", Dispatchable: true}, {Name: "Done", Terminal: true}}
	scope := hubscope.NativeScope{Organization: "org_security", Project: f.project}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), scope.Project)
		if err != nil {
			return err
		}
		project.States = states
		if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		_, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: scope.Organization, ID: "prj_other", Name: "foreign-project-sentinel", Profile: "github_compatible"}, States: states})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	other := f
	other.project = "prj_other"
	issue := seedAnalyticsTestWorkItem(t, f.service, scope.Organization, scope.Project, "analytics-residence", 1, "lane residence", "Todo", now.Add(-6*time.Minute))
	transitionAnalyticsTestWorkItem(t, f.service, scope, issue.WorkItemID, "In Progress", now.Add(-2*time.Minute))
	call := func(user hostedSecurityUser, name string, args map[string]any) (*httptest.ResponseRecorder, json.RawMessage) {
		t.Helper()
		payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args, "_meta": json.RawMessage(fleetProtocolMeta)}}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", name)
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
		request.Header.Set("X-CSRF-Token", hostedCSRF(user.token))
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		var envelope struct {
			Result struct {
				Data json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("response %s", response.Body.String())
		}
		return response, envelope.Result.Data
	}
	for _, grant := range []string{"native", "github", "both"} {
		user := f.user(t, grant, "member", grant+"@example.test", "", "")
		if grant != "github" {
			f.grant(t, user, false, false)
		}
		if grant != "native" {
			other.grant(t, user, false, false)
		}
		for _, name := range []string{operatortool.Dashboard, operatortool.BoardState} {
			t.Run(grant+"/"+name, func(t *testing.T) {
				response, raw := call(user, name, map[string]any{})
				var board nativeBoardResult
				if err := json.Unmarshal(raw, &board); err != nil || response.Code != http.StatusOK || board.GeneratedAt.IsZero() {
					t.Fatalf("hosted board: %s err=%v", response.Body.String(), err)
				}
				want := 1
				if grant == "github" {
					want = 0
				}
				if len(board.Projects) != want || strings.Contains(string(raw), "prj_other") || grant == "github" && strings.Contains(string(raw), string(f.project)) {
					t.Fatalf("hosted board project authority: %s", raw)
				}
			})
		}
		t.Run(grant+"/organization reports", func(t *testing.T) {
			for _, filter := range []bool{false, true} {
				args := map[string]any{"scope": "organization", "range": "24h"}
				if filter {
					args["projects"] = []string{string(f.project), "prj_ungranted"}
					args["types"] = []string{"bug"}
				}
				response, raw := call(user, operatortool.Reports, args)
				var report postgresOrganizationProductivity
				if err := json.Unmarshal(raw, &report); err != nil || response.Code != http.StatusOK || report.OrganizationID != "org_security" {
					t.Fatalf("organization response=%s err=%v", response.Body.String(), err)
				}
				want := 1
				if grant == "both" && !filter {
					want = 2
				}
				if grant == "github" && filter {
					want = 0
				}
				if len(report.Projects) != want {
					t.Fatalf("project count=%d want=%d", len(report.Projects), want)
				}
				if grant == "native" && strings.Contains(string(raw), "prj_other") || grant == "github" && strings.Contains(string(raw), string(f.project)) || strings.Contains(string(raw), "prj_ungranted") {
					t.Fatalf("organization leaked project: %s", raw)
				}
				assertProductivitySums(t, report)
			}
		})
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
			t.Run(grant+"/"+name, func(t *testing.T) {
				response, raw := call(user, name, map[string]any{})
				if response.Code != http.StatusOK || len(raw) == 0 {
					t.Fatalf("read %s", response.Body.String())
				}
				var report nativeAnalyticsReport
				if err := json.Unmarshal(raw, &report); err != nil {
					t.Fatal(err)
				}
				if report.Requests != nil || report.RouteLatency != nil {
					t.Fatalf("member received tenant-wide traffic: %s", raw)
				}
				want := 1
				if grant != "github" && name == operatortool.Reports {
					for _, filter := range []struct {
						kind string
						want int
					}{{"bug", 1}, {"change_request", 0}} {
						filteredResponse, filteredRaw := call(user, name, map[string]any{"projects": []string{string(f.project)}, "types": []string{filter.kind}})
						var filtered nativeAnalyticsReport
						if err := json.Unmarshal(filteredRaw, &filtered); err != nil || filteredResponse.Code != http.StatusOK || len(filtered.Projects) != 1 {
							t.Fatalf("filtered aggregate=%s error=%v", filteredResponse.Body.String(), err)
						}
						project := filtered.Projects[0]
						if project.Quality.WorkedItems != filter.want || len(project.LaneResidence.Issues.Items) != filter.want || project.LaneResidence.SystemTotal.Seconds != float64(filter.want*360) {
							t.Fatalf("%s cohort %s=%s", name, filter.kind, filteredRaw)
						}
					}
				}
				if grant == "both" {
					want = 2
				}
				if !strings.Contains(string(raw), `"failure_signatures":`) || len(report.Projects) != want || report.ObservedAt.IsZero() || report.OrganizationID != "org_security" {
					t.Fatalf("population %s", raw)
				}
				if grant == "native" && strings.Contains(string(raw), "prj_other") || grant == "github" && strings.Contains(string(raw), string(f.project)) {
					t.Fatalf("cross-project data %s", raw)
				}
				for _, project := range report.Projects {
					if project.ProjectID != string(f.project) {
						continue
					}
					if project.Quality.WorkedItems != 1 || project.Quality.ReworkedItems != 0 || project.Quality.ReworkPercent == nil || *project.Quality.ReworkPercent != 0 || project.Quality.EscapePercent != nil {
						t.Fatalf("MCP quality scope and null denominator %s", raw)
					}
					if project.LaneResidence.SystemTotal.Seconds != 360 || project.QueueTime.Seconds != 240 || len(project.LaneResidence.Issues.Items) != 1 || len(project.LaneResidence.Aging.Items) != 1 {
						t.Fatalf("MCP residence %s", raw)
					}
					var queueSeconds float64
					for _, bucket := range project.Digest {
						queueSeconds += bucket.QueueSeconds
					}
					if queueSeconds != 240 {
						t.Fatalf("MCP queue buckets %s", raw)
					}
				}
				if grant == "both" {
					for offset := range 2 {
						response, raw := call(user, name, map[string]any{"limit": 1, "offset": offset})
						var page nativeAnalyticsReport
						if err := json.Unmarshal(raw, &page); err != nil || len(page.Projects) != 1 || (page.NextOffset != nil) != (offset == 0) {
							t.Fatalf("native project page %d: %s error=%v", offset, response.Body.String(), err)
						}
					}
				}
				request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
				c := f.service.echo.NewContext(request, httptest.NewRecorder())
				c.SetPath("/mcp")
				authorityContexts := make(chan context.Context, 1)
				if err := f.service.operatorAuthority(func(c echo.Context) error {
					authorityContexts <- c.Request().Context()
					return nil
				})(c); err != nil {
					t.Fatal(err)
				}
				if len(authorityContexts) == 0 {
					t.Fatal("current application authority unavailable")
				}
				authorityContext := context.WithoutCancel(<-authorityContexts)
				authorityContext = operatortool.BindConnection(authorityContext, "analytics-"+grant+"-"+name, "analytics-test")
				stdio := nativeAnalyticsStdioCall(t, authorityContext, hostedOperatorExecutor{f.service}, name)
				if string(stdio) != string(raw) {
					t.Fatalf("stdio/HTTP application divergence:\n%s\n%s", stdio, raw)
				}
				if name == operatortool.TimeSeries && grant != "github" {
					for _, interval := range []struct {
						from, to     time.Time
						queueSeconds float64
					}{
						{now.Add(-6 * time.Minute), now.Add(-3 * time.Minute), 180},
						{now.Add(-49 * time.Hour), now.Add(-48 * time.Hour), 0},
					} {
						response, history := call(user, name, map[string]any{"project_id": f.project, "from": interval.from.Format(time.RFC3339Nano), "to": interval.to.Format(time.RFC3339Nano), "bucket": "1m"})
						var report nativeAnalyticsReport
						if err := json.Unmarshal(history, &report); err != nil || response.Code != http.StatusOK || len(report.Projects) != 1 {
							t.Fatalf("historical read failed: %s, %v", response.Body.String(), err)
						}
						var queueSeconds float64
						for _, bucket := range report.Projects[0].Digest {
							if bucket.From.Before(interval.from) || bucket.To.After(interval.to) {
								t.Fatalf("bucket outside requested interval: %+v", bucket)
							}
							queueSeconds += bucket.QueueSeconds
						}
						if queueSeconds != interval.queueSeconds || !report.Window.From.Equal(interval.from) || !report.Window.To.Equal(interval.to) {
							t.Fatalf("historical queue read: %s", history)
						}
					}
				}
				for _, args := range []map[string]any{{"from": now.Add(-time.Hour).Format(time.RFC3339Nano), "to": now.Add(time.Hour).Format(time.RFC3339Nano)}, {"from": now.Add(-91 * 24 * time.Hour).Format(time.RFC3339Nano), "to": now.Format(time.RFC3339Nano)}, {"from": now.Add(-time.Hour).Format(time.RFC3339Nano), "to": now.Add(-2 * time.Hour).Format(time.RFC3339Nano)}, {"limit": 201}, {"window": "25h"}, {"from": "bad"}, {"bucket": "1ns"}} {
					response, raw := call(user, name, args)
					if len(raw) != 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
						t.Fatalf("invalid window accepted: %s", response.Body.String())
					}
				}
				selected := string(f.project)
				if grant == "github" {
					selected = "prj_other"
				}
				response, raw = call(user, name, map[string]any{"project_id": selected, "limit": 1})
				if len(raw) == 0 {
					t.Fatalf("selected %s", response.Body.String())
				}
			})
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), user.identity.Subject)
			if err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{f.project, other.project} {
				if err := w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, project); err != nil {
					return err
				}
				if err := w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, project); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports, operatortool.Dashboard, operatortool.BoardState} {
			response, raw := call(user, name, map[string]any{"project_id": string(f.project)})
			if len(raw) > 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
				t.Fatalf("revoked grant %s", response.Body.String())
			}
		}
	}

	m := f.service.requestMetrics
	m.Record(now.Add(-time.Minute), "GET /items/:item", "runner", "runner_report", 200, 10*time.Millisecond)
	if err := m.Flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	f.grant(t, owner, false, false)
	response, raw := call(owner, operatortool.Reports, map[string]any{})
	var report nativeAnalyticsReport
	if err := json.Unmarshal(raw, &report); err != nil || response.Code != http.StatusOK || report.Requests == nil || len(report.Requests.Callers) != 1 || report.Requests.Callers[0].Caller != "runner_report" {
		t.Fatalf("admin MCP traffic: %s err=%v", response.Body.String(), err)
	}
	response, raw = call(owner, operatortool.TimeSeries, map[string]any{"window": "2h", "bucket": "1h"})
	report = nativeAnalyticsReport{}
	if err := json.Unmarshal(raw, &report); err != nil || response.Code != http.StatusOK || report.RouteLatency == nil {
		t.Fatalf("admin MCP route latency: %s err=%v", response.Body.String(), err)
	}
	recorded := slices.IndexFunc(report.RouteLatency.Buckets, func(bucket routeLatencyBucket) bool { return bucket.Route == "GET /items/:item" })
	if recorded < 0 || report.RouteLatency.Buckets[recorded].StatusClass != "2xx" || report.RouteLatency.Buckets[recorded].Count != 1 || report.RouteLatency.Buckets[recorded].P95 != 10 {
		t.Fatalf("admin MCP route latency buckets: %+v", report.RouteLatency.Buckets)
	}
	t.Run("analytics reads beside a held writer", func(t *testing.T) {
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		if f.service.outbox != nil {
			f.service.outbox.stop()
		}
		f.service.workerCancel()
		<-f.service.workerDone
		<-f.service.secretRewrapDone
		poolConfig, err := f.service.config.Postgres.Connection.PoolConfig()
		if err != nil {
			t.Fatal(err)
		}
		poolConfig.MaxConns = 4
		var guarded *postgres.Repository
		poolConfig.ConnConfig.Tracer = analyticsReadPoolTrace{}
		pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
		if err != nil {
			t.Fatal(err)
		}
		guarded, err = postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1})
		if err != nil {
			pool.Close()
			t.Fatal(err)
		}
		oldTenant, oldAccess, oldBilling := f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.config.BillingRepository
		oldDatabase, oldMetrics, oldDatabaseBilling := f.service.database.postgres, f.service.requestMetrics.Repository, f.service.database.billingRepository
		f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.config.BillingRepository = guarded, guarded, guarded
		f.service.database.postgres, f.service.requestMetrics.Repository, f.service.database.billingRepository = guarded, guarded, guarded
		defer func() {
			f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.config.BillingRepository = oldTenant, oldAccess, oldBilling
			f.service.database.postgres, f.service.requestMetrics.Repository, f.service.database.billingRepository = oldDatabase, oldMetrics, oldDatabaseBilling
			_ = guarded.Close()
		}()
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(t.Context())
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: owner.token})
		c := f.service.echo.NewContext(request, httptest.NewRecorder())
		c.SetPath("/mcp")
		authorityContexts := make(chan context.Context, 1)
		if err := f.service.operatorAuthority(func(c echo.Context) error {
			authorityContexts <- c.Request().Context()
			return nil
		})(c); err != nil || len(authorityContexts) == 0 {
			t.Fatalf("operator authority: %v", err)
		}
		authorityContext := operatortool.BindConnection(<-authorityContexts, "analytics-held-writer", "analytics-test")
		credential, err := currentHubOperator(authorityContext)
		if err != nil {
			t.Fatal(err)
		}
		type projectReportResult struct {
			report hostedReportsReport
			found  bool
		}
		readProjectReport := func(ctx context.Context) (any, error) {
			projects, found, err := f.service.reportsReadableProjects(ctx, credential, string(f.project))
			if err != nil || !found {
				return projectReportResult{found: found}, err
			}
			result, err := f.service.readPostgresHostedReport(ctx, credential, projects, report.Window, reportsFilter{}, true)
			return projectReportResult{report: result, found: found}, err
		}
		for _, read := range []struct {
			name string
			run  func(context.Context) (any, error)
		}{
			{"analytics", func(ctx context.Context) (any, error) {
				return f.service.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: string(f.project), Limit: 1}, report.Window)
			}},
			{"diagnostics", func(ctx context.Context) (any, error) {
				return f.service.readCloudDiagnostics(ctx, credential, report.Window)
			}},
			{"reports", readProjectReport},
			{"analytics revoked grants", func(ctx context.Context) (any, error) {
				return f.service.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: string(f.project), Limit: 1}, report.Window)
			}},
			{"reports revoked grants", readProjectReport},
		} {
			t.Run(read.name, func(t *testing.T) {
				if read.name == "analytics revoked grants" {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
						member, err := w.AccessMember(t.Context(), owner.identity.Subject)
						member.Role = "member"
						return errors.Join(err, w.UpdateAccessMember(t.Context(), member), w.DeleteAccessProjectGrant(t.Context(), owner.identity.Subject, scope.Project))
					}); err != nil {
						t.Fatal(err)
					}
				}
				want, err := read.run(authorityContext)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(testenv.WaitContext(t, authorityContext))
				defer cancel()
				writer, err := hostedPostgresAdmin(t, f.service).BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback()
				if _, err := writer.ExecContext(ctx, "UPDATE projects SET name=name WHERE organization_id=$1 AND id=$2", scope.Organization, scope.Project); err != nil {
					t.Fatal(err)
				}
				repository := f.service.config.TenantRepository.(*postgres.Repository)
				connection := operatortool.CurrentConnection(ctx)
				resolve := connection.Resolve
				connection.Resolve = func(ctx context.Context) (operatortool.Authority, error) {
					if held := repository.Stats().AcquiredConns(); held != 0 {
						return operatortool.Authority{}, fmt.Errorf("analytics authority retained %d PostgreSQL connections", held)
					}
					authority, err := resolve(ctx)
					if err != nil {
						return authority, err
					}
					check := authority.Check
					authority.Check = func(ctx context.Context, requirement operatortool.Requirement) error {
						if held := repository.Stats().AcquiredConns(); held != 0 {
							return fmt.Errorf("analytics authorization retained %d PostgreSQL connections", held)
						}
						return check(ctx, requirement)
					}
					return authority, nil
				}
				ctx = operatortool.WithConnection(ctx, connection)
				guard := &analyticsReadAcquireGuard{cancel: cancel}
				ctx = context.WithValue(ctx, analyticsReadAcquireKey{}, guard)
				got, err := read.run(ctx)
				if guard.nested.Load() {
					t.Fatal("analytics read requested another reader while holding the only connection")
				}
				if err != nil {
					t.Fatalf("read beside held writer: %v", err)
				}
				if analytics, ok := got.(nativeAnalyticsReport); ok {
					wantProjects := 1
					if read.name == "analytics revoked grants" {
						wantProjects = 0
					}
					if len(analytics.Projects) != wantProjects {
						t.Fatalf("authorized projects = %d, want %d", len(analytics.Projects), wantProjects)
					}
				}
				if result, ok := got.(projectReportResult); ok {
					if found := read.name == "reports"; result.found != found {
						t.Fatalf("project report readable = %t, want %t", result.found, found)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("response changed beside held writer:\ngot: %+v\nwant: %+v", got, want)
				}
			})
		}
	})
	t.Run("reader snapshot holds analytics population", func(t *testing.T) {
		f := newDefaultNativeFixture(t, Config{})
		f.create(t, "first")
		f.create(t, "second")
		scope := analyticsTestScope(t, f)
		now := f.service.config.Clock().UTC()
		window := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
		owner := f.service.config.TenantRepository.(tenantstore.SnapshotRepository)
		if err := owner.ReadTenantSnapshot(t.Context(), scope.Organization, corestore.Background, func(reader tenantstore.Reader) error {
			principal, err := f.service.analyticsSnapshotPrincipal(t.Context(), reader, scope)
			if err != nil {
				return err
			}
			read := func() (int, error) {
				page, err := reader.(eventstore.AnalyticsResidenceReader).ReadAnalyticsResidencePage(t.Context(), eventstore.AnalyticsResidenceQuery{ProjectID: scope.Project, Principal: principal, From: window.From, To: window.To, Limit: 10})
				return len(page.Issues), err
			}
			if got, err := read(); err != nil || got != 2 {
				t.Fatalf("initial analytics population = %d, want 2: %v", got, err)
			}
			f.create(t, "third")
			if got, err := read(); err != nil || got != 2 {
				t.Fatalf("held analytics snapshot after write = %d, want 2: %v", got, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		report, err := f.service.readNativeAnalyticsTenant(t.Context(), scope, operatortool.AnalyticsRequest{Limit: 10}, window)
		if err != nil || report.LaneResidence.IssuesObserved != 3 {
			t.Fatalf("committed population = %d, want 3: %v", report.LaneResidence.IssuesObserved, err)
		}
	})

}

func TestNativeAnalyticsUnavailableServices(t *testing.T) {
	t.Parallel()
	identity := operatortool.Identity{PrincipalID: "test", OrganizationID: "org_test", CredentialID: "test"}
	ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
		return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
	}})
	ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(context.Context) (hubapi.APICredential, error) { return hubapi.APICredential{}, nil })
	for _, cfg := range []Config{{Clock: time.Now}, {Clock: time.Now, Hosted: &hostedplans.HostedConfig{OrganizationID: "org_test"}}} {
		service := &Service{config: cfg}
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
			_, err := service.executeAnalyticsRead(ctx, operatortool.Call{Name: name, Arguments: []byte(`{}`)})
			if !errors.Is(err, errHubOperatorUnavailable) {
				t.Fatalf("missing service %s: %v", name, err)
			}
			_, err = service.executeAnalyticsRead(t.Context(), operatortool.Call{Name: name, Arguments: []byte(`{}`)})
			if !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("missing authority %s: %v", name, err)
			}
		}
	}
	_, err := hubOperatorResult(nativeAnalyticsProject{SkipReasons: []nativeAnalyticsSkip{{Reason: strings.Repeat("x", operatortool.MaxResultBytes)}}})
	if !errors.Is(err, errHubOperatorUnavailable) {
		t.Fatalf("oversized native report: %v", err)
	}
	t.Run("receipt pages retain offsets under byte limit", func(t *testing.T) {
		attempts := make([]nativeAnalyticsAttempt, 200)
		sources := make([]workflowmetrics.InstructionRef, 64)
		for i := range sources {
			sources[i] = workflowmetrics.InstructionRef{Name: "AGENTS.md (effective)", Hash: strings.Repeat("a", 64), PathRef: strings.Repeat("b", 64), Version: strings.Repeat("c", 64), ObservedAt: time.Now().UTC()}
		}
		for i := range attempts {
			attempts[i] = nativeAnalyticsAttempt{Activity: &workflowmetrics.ActivityProfile{Sources: sources}, Pipeline: make([]gate.PipelineTiming, 1024)}
		}
		timings := make([]gate.PipelineTiming, 200)
		for i := range timings {
			timings[i] = (gate.CommandResult{Command: "make " + strings.Repeat("v", 3000), StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40)}).PipelineTiming("finalization")
		}
		for offset := 0; offset < len(timings); {
			page := pipelineTimingPage(timings, offset, 200)
			raw, err := json.Marshal(page.Items)
			if err != nil || len(raw) > operatortool.WorkListPageBytes || len(page.Items) == 0 || page.Offset != offset {
				t.Fatalf("timing page offset=%d size=%d err=%v", offset, len(raw), err)
			}
			offset += len(page.Items)
			if offset < len(timings) && (page.NextOffset == nil || *page.NextOffset != offset) || offset == len(timings) && page.NextOffset != nil {
				t.Fatalf("lost timing continuation %#v", page)
			}
		}
		offset := 0
		for offset < len(attempts) {
			page := nativeAnalyticsAttemptPage(attempts, offset, 200)
			raw, err := json.Marshal(page.Items)
			if err != nil || len(raw) > operatortool.WorkListPageBytes || len(page.Items) == 0 || page.Offset != offset {
				t.Fatalf("bounded page offset=%d size=%d err=%v", offset, len(raw), err)
			}
			offset += len(page.Items)
			if offset < len(attempts) && (page.NextOffset == nil || *page.NextOffset != offset) || offset == len(attempts) && page.NextOffset != nil {
				t.Fatalf("lost page continuation %#v", page)
			}
		}
	})
	t.Run("typed attempt objects counts and confidential continuation", func(t *testing.T) {
		details := &memoryRunDetails{}
		f := newDefaultNativeFixture(t, Config{RunDetails: details, SecretKeys: secretTestKeys(t, "1", "1")})
		approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
		issue := f.create(t, "typed analytics attempt")
		worker := f.worker(t, "typed analytics")
		lease := claimNativeAttempt(t, f, worker, "analytics-typed-machine", "analytics-typed-session", issue.WorkItemID)
		event := nativeStartedEvent(lease)
		event.Data.Identity.Role = "merge"
		base := f.service.config.Clock().UTC().Truncate(time.Microsecond)
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: base, Phases: []tracker.NativePhase{{Name: "planning", StartedAt: base.Add(-time.Minute), FinishedAt: base}}, Identity: agentidentity.RuntimeUpdate("merge-model", "openai", "high", "", base), Pipeline: []gate.PipelineTiming{gate.Interval("rebase", base.Add(-time.Minute), base, "clean")}}
		event.Data.Runtime.Identity.Role = "merge"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, event), http.StatusOK)
		credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
		if err != nil || status != http.StatusOK {
			t.Fatalf("analytics authority status=%d error=%v", status, err)
		}
		scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
		repository := f.service.config.TenantRepository.(*postgres.Repository)
		record := func(index int) reportstore.UsageRecord {
			return reportstore.UsageRecord{OrganizationID: scope.Organization, UpdatedAt: base, UsageFact: reportstore.UsageFact{ProjectID: scope.Project, WorkItemID: issue.WorkItemID, AttemptID: event.Data.AttemptID, Period: base.Add(time.Duration(index) * time.Millisecond), Provider: "example", Model: "example-model", Currency: "USD", Input: 1}}
		}
		if err := repository.ImportReportUsageFact(t.Context(), record(0)); err != nil {
			t.Fatal(err)
		}
		window := operatortool.AnalyticsWindow{From: base.Add(-time.Hour), To: base.Add(time.Hour), Bucket: time.Hour}
		readUsage := func() []reportstore.UsageFact {
			t.Helper()
			var facts []reportstore.UsageFact
			if err := repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				facts, err = r.(reportstore.UsageReader).ReadReportUsage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{scope.Project}, From: window.From, To: window.To, Limit: 1001})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return facts
		}
		var objectReads atomic.Int64
		details.beforeRead = func(context.Context) error {
			if held := repository.Stats().AcquiredConns(); held != 0 {
				return fmt.Errorf("analytics object read retained %d PostgreSQL connections", held)
			}
			objectReads.Add(1)
			return nil
		}
		population, next, err := f.service.readNativeAnalyticsAttemptPopulationTenant(t.Context(), scope, window, "", readUsage(), false)
		if err != nil || len(population.Items) != 1 || next != "" || population.Partial || population.Clipped {
			t.Fatalf("typed attempt population=%+v next=%q error=%v", population, next, err)
		}
		attempt := population.Items[0]
		if attempt.AttemptID != event.Data.AttemptID || attempt.WorkItemID != string(issue.WorkItemID) || attempt.Identity.Role != "merge" || attempt.Identity.ResolvedModel.Value != "merge-model" || len(attempt.Phases) != 1 || len(attempt.Pipeline) != 1 || attempt.UsageRowsObserved != 1 || objectReads.Load() == 0 {
			t.Fatalf("typed attempt receipt=%+v object reads=%d", attempt, objectReads.Load())
		}
		details.beforeRead = nil
		for start := 1; start <= 1000; start += 100 {
			records := []reportstore.UsageRecord{}
			for index := start; index < start+100 && index <= 1000; index++ {
				records = append(records, record(index))
			}
			if err := repository.ImportReportUsageFacts(t.Context(), records); err != nil {
				t.Fatal(err)
			}
		}
		usage := readUsage()
		if len(usage) != 1001 {
			t.Fatalf("bounded actual usage population=%d", len(usage))
		}
		clipped, _, err := f.service.readNativeAnalyticsAttemptPopulationTenant(t.Context(), scope, window, "", usage, false)
		if err != nil || len(clipped.Items) != 1 || clipped.Items[0].UsageRowsObserved != 1000 || !clipped.Partial || !clipped.Clipped {
			t.Fatalf("clipped observed usage=%+v error=%v", clipped, err)
		}
		usagePage, usageNext, err := f.service.readNativeAnalyticsUsagePopulationTenant(t.Context(), scope, window)
		if err != nil || len(usagePage.Items) != 1000 || usagePage.Examined != 1001 || !usagePage.Partial || usageNext == "" {
			t.Fatalf("typed usage head rows=%d examined=%d partial=%v next=%q error=%v", len(usagePage.Items), usagePage.Examined, usagePage.Partial, usageNext, err)
		}
		usageCtx, _, err := analyticsPagingContext(t.Context(), usageNext)
		if err != nil {
			t.Fatal(err)
		}
		usageTail, usageDone, err := f.service.readNativeAnalyticsUsagePopulationTenant(usageCtx, scope, window)
		if err != nil || len(usageTail.Items) != 1 || usageTail.Items[0].AttemptID != event.Data.AttemptID || !usageTail.Items[0].Period.Equal(base.Add(time.Second)) || usageTail.Examined != 1 || !usageTail.Partial || usageDone != "" {
			t.Fatalf("typed usage tail=%+v next=%q error=%v", usageTail, usageDone, err)
		}
		associated, _, err := f.service.readNativeAnalyticsAttemptPopulationTenant(t.Context(), scope, window, "", usagePage.Items, usagePage.Partial)
		if err != nil || len(associated.Items) != 1 || associated.Items[0].UsageRowsObserved != 1000 || !associated.Partial || !associated.Clipped {
			t.Fatalf("bounded usage association=%+v error=%v", associated, err)
		}
		usageParts := strings.Split(usageNext, ":")
		usageSigned := strings.Split(usageParts[1], ".")
		usagePayload, err := base64.RawURLEncoding.DecodeString(usageSigned[0])
		if err != nil || strings.Contains(string(usagePayload), event.Data.AttemptID) {
			t.Fatalf("usage cursor confidentiality error=%v", err)
		}
		for _, change := range []string{"project", "caller", "window"} {
			changedScope, changedWindow := scope, window
			switch change {
			case "project":
				changedScope.Project = "different-project"
			case "caller":
				changedScope.Credential.ID = "different-caller"
			case "window":
				changedWindow.From = window.From.Add(time.Second)
			}
			_, _, decodedKey, err := f.service.analyticsUsageCursor(usageCtx, changedScope, changedWindow)
			clear(decodedKey)
			if err == nil {
				t.Fatalf("usage continuation accepted changed %s", change)
			}
		}
		usageAfter, usageCursor, usageKey, err := f.service.analyticsUsageCursor(usageCtx, scope, window)
		if err != nil {
			t.Fatal(err)
		}
		if usageAfter.Source != 1 || usageAfter.SourceKey != "USD" {
			t.Fatalf("historical currency tie key=%+v", usageAfter)
		}
		oversizedUsage := usageAfter
		oversizedUsage.SourceKey = strings.Repeat("x", 4096)
		if token, err := f.service.encodeAnalyticsUsageCursor(usageCursor, usageKey, oversizedUsage); err == nil || token != "" {
			t.Fatalf("oversized encrypted usage cursor length=%d error=%v", len(token), err)
		}
		clear(usageKey)
		emptyFixture := newNativeFixture(t, f.service, scope.Organization, "historical empty usage dimensions")
		emptyScope := analyticsTestScope(t, emptyFixture)
		emptyItem := seedAnalyticsTestWorkItem(t, f.service, emptyScope.Organization, emptyScope.Project, "empty-dimensions", 1, "empty dimensions", "Todo", base.Add(-time.Hour))
		emptyAttempt := "empty-" + string(emptyScope.Project)
		emptyDB := hostedPostgresAdmin(t, f.service)
		seedAnalyticsTestAttempt(t, emptyDB, f.service, emptyScope, emptyItem.WorkItemID, emptyAttempt, "succeeded", base.Add(-time.Minute), base, tracker.NativeRunData{Identity: &tracker.NativeExecutionIdentity{Role: "code"}})
		var workedReads atomic.Int64
		details.beforeRead = func(context.Context) error {
			if held := repository.Stats().AcquiredConns(); held != 0 {
				return fmt.Errorf("worked role object read retained %d PostgreSQL connections", held)
			}
			workedReads.Add(1)
			return nil
		}
		worked, workedPartial, workedUnavailable, err := f.service.readAnalyticsWorkedPopulationTenant(t.Context(), emptyScope, window)
		details.beforeRead = nil
		if err != nil || workedPartial || len(workedUnavailable) != 0 || len(worked) != 1 || worked[0].Item != string(emptyItem.WorkItemID) || worked[0].Kind != "worked" || !worked[0].At.Equal(base.Add(-time.Minute)) || workedReads.Load() == 0 {
			t.Fatalf("declared-role worked population=%+v partial=%v unavailable=%v reads=%d error=%v", worked, workedPartial, workedUnavailable, workedReads.Load(), err)
		}
		if _, err := emptyDB.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,$3,$4::timestamptz+n*interval '1 millisecond','','',1,0,0,0,'USD',$4 FROM generate_series(0,1000)n`, emptyScope.Organization, emptyScope.Project, emptyAttempt, base); err != nil {
			t.Fatal(err)
		}
		emptyHead, emptyNext, err := f.service.readNativeAnalyticsUsagePopulationTenant(t.Context(), emptyScope, window)
		if err != nil || len(emptyHead.Items) != 1000 || !emptyHead.Partial || emptyNext == "" {
			t.Fatalf("empty usage head=%+v next=%q error=%v", emptyHead, emptyNext, err)
		}
		emptyCtx, _, err := analyticsPagingContext(t.Context(), emptyNext)
		if err != nil {
			t.Fatal(err)
		}
		emptyTail, emptyDone, err := f.service.readNativeAnalyticsUsagePopulationTenant(emptyCtx, emptyScope, window)
		if err != nil || len(emptyTail.Items) != 1 || emptyTail.Items[0].Provider != "" || emptyTail.Items[0].Model != "" || !emptyTail.Items[0].Period.Equal(base.Add(time.Second)) || emptyDone != "" || !emptyTail.Partial {
			t.Fatalf("empty dimensions continuation=%+v next=%q error=%v", emptyTail, emptyDone, err)
		}
		withoutKeys := &Service{config: f.service.config, database: f.service.database, localOrganization: f.service.localOrganization}
		withoutKeys.config.SecretKeys = nil
		firstWithoutKeys, noKeyNext, err := withoutKeys.readNativeAnalyticsUsagePopulationTenant(t.Context(), scope, window)
		if err != nil || len(firstWithoutKeys.Items) != 1000 || !firstWithoutKeys.Partial || !firstWithoutKeys.ContinuationUnavailable || noKeyNext != "" {
			t.Fatalf("no-keyring first usage page rows=%d partial=%v unavailable=%v next=%q error=%v", len(firstWithoutKeys.Items), firstWithoutKeys.Partial, firstWithoutKeys.ContinuationUnavailable, noKeyNext, err)
		}
		if _, _, err := withoutKeys.readNativeAnalyticsUsagePopulationTenant(usageCtx, scope, window); err == nil {
			t.Fatal("no-keyring service accepted encrypted usage continuation")
		}
		noKeyAttempts, _, err := withoutKeys.readNativeAnalyticsAttemptPopulationTenant(t.Context(), scope, window, "", firstWithoutKeys.Items, firstWithoutKeys.Partial)
		if err != nil || len(noKeyAttempts.Items) != 1 || noKeyAttempts.Items[0].UsageRowsObserved != 1000 || !noKeyAttempts.Partial || !noKeyAttempts.Clipped {
			t.Fatalf("no-keyring actual attempts=%+v error=%v", noKeyAttempts, err)
		}
		content := eventstore.NewContent(details)
		for _, source := range []struct {
			id, source, outcome, reason string
			at                          time.Time
		}{{"analytics-capacity-skip", "native_provider_capacity", "skipped", "capacity", base.Add(time.Minute)}, {"analytics-capacity-claim", "native_claim", "claimed", "owned", base.Add(2 * time.Minute)}} {
			raw, err := json.Marshal(tracker.CollaborationEvent{ID: source.id, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: issue.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "analytics-runner"}, RecordedAt: source.at, Data: tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: source.source, Outcome: source.outcome, Reason: source.reason, At: source.at, RunnerID: "analytics-runner", WorkItemRevision: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), scope.Organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
				t.Fatal(err)
			}
		}
		scheduler := nativeAnalyticsProject{Window: window, Digest: []nativeAnalyticsBucket{{From: base, To: base.Add(time.Hour)}}}
		details.beforeRead = func(context.Context) error {
			if held := repository.Stats().AcquiredConns(); held != 0 {
				return fmt.Errorf("scheduler object read retained %d PostgreSQL connections", held)
			}
			return nil
		}
		if err := f.service.projectNativeAnalyticsSkipsTenant(t.Context(), scope, &scheduler); err != nil {
			t.Fatal(err)
		}
		if scheduler.DecisionsObserved != 1 || len(scheduler.SkipReasons) != 1 || scheduler.SkipReasons[0].Source != "native_provider_capacity" || scheduler.SkipReasons[0].Reason != "capacity" || scheduler.SkipReasons[0].Count != 1 {
			t.Fatalf("typed scheduler skips=%+v", scheduler.SkipReasons)
		}
		if err := f.service.projectNativeAnalyticsCapacityWaitTenant(t.Context(), scope, &scheduler); err != nil {
			t.Fatal(err)
		}
		if scheduler.ProviderCapacityWait.ClaimsObserved != 2 || scheduler.ProviderCapacityWait.IntervalsObserved != 1 || scheduler.ProviderCapacityWait.IntervalsUnavailable != 1 || scheduler.ProviderCapacityWait.Seconds != 60 || scheduler.Digest[0].ProviderCapacityWaitSeconds != 60 {
			t.Fatalf("typed scheduler capacity=%+v digest=%+v", scheduler.ProviderCapacityWait, scheduler.Digest)
		}
		details.beforeRead = nil
		_, cursor, key, err := f.service.analyticsAttemptCursor(t.Context(), scope, window, "", "attempts")
		if err != nil {
			t.Fatal(err)
		}
		defer clear(key)
		hidden := reportstore.AnalyticsAttemptCursor{StartedAt: base, Submicro: 7, ID: "unreadable-attempt-sentinel"}
		token, err := f.service.encodeAnalyticsAttemptCursor(cursor, key, hidden, "attempts")
		if err != nil {
			t.Fatal(err)
		}
		if len(token) > 4096 {
			t.Fatalf("typed continuation length=%d", len(token))
		}
		parts := strings.Split(token, ":")
		signed := strings.Split(parts[1], ".")
		payload, err := base64.RawURLEncoding.DecodeString(signed[0])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), hidden.ID) || strings.Contains(token, hidden.ID) {
			t.Fatal("continuation exposed an unreadable attempt identity")
		}
		ctx, _, err := analyticsPagingContext(t.Context(), token)
		if err != nil {
			t.Fatal(err)
		}
		after, _, decodedKey, err := f.service.analyticsAttemptCursor(ctx, scope, window, "", "attempts")
		clear(decodedKey)
		if err != nil || after.ID != hidden.ID || after.Submicro != 7 || !after.StartedAt.Equal(base) {
			t.Fatalf("typed seek continuation=%+v error=%v", after, err)
		}
		for _, change := range []string{"project", "caller", "role", "window", "runner"} {
			changedScope, changedWindow, runner := scope, window, ""
			switch change {
			case "project":
				changedScope.Project = "different-project"
			case "caller":
				changedScope.Credential.ID = "different-caller"
			case "role":
				changedScope.Credential.HostedRole = "reporter"
			case "window":
				changedWindow.From = window.From.Add(time.Second)
			case "runner":
				runner = "different-runner"
			}
			_, _, decodedKey, err := f.service.analyticsAttemptCursor(ctx, changedScope, changedWindow, runner, "attempts")
			clear(decodedKey)
			if err == nil {
				t.Fatalf("continuation accepted changed %s authority/filter", change)
			}
		}
	})

}

func nativeAnalyticsStdioCall(t *testing.T, ctx context.Context, executor mcp.Executor, name string) json.RawMessage {
	t.Helper()
	input, inputWriter := io.Pipe()
	output, outputWriter := io.Pipe()
	done := make(chan error, 1)
	t.Cleanup(func() { input.Close(); inputWriter.Close(); output.Close(); outputWriter.Close() })
	go func() { done <- mcp.NewServer(executor, "test").Serve(ctx, input, outputWriter) }()
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{},"_meta":` + fleetProtocolMeta + `}}` + "\n"
	if _, err := io.WriteString(inputWriter, request); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Data json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	var frame json.RawMessage
	if err := json.NewDecoder(output).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Data) == 0 || string(envelope.Result.Data) == "null" {
		t.Fatalf("stdio frame=%s", frame)
	}
	return envelope.Result.Data
}

func TestNativeAnalyticsCostPerOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name               string
		usageRows          int
		landings           int
		sharedAttempt      bool
		usageAtWindowEnd   bool
		partialHour        bool
		clipped            bool
		populationObserved int
		costPerShipped     float64
		tokensPerShipped   float64
	}{
		{name: "complete aligned population", usageRows: 2, landings: 2, populationObserved: 2, costPerShipped: 4, tokensPerShipped: 120},
		{name: "multiple rows from one attempt", usageRows: 2, landings: 2, sharedAttempt: true, populationObserved: 1, costPerShipped: 4, tokensPerShipped: 120},
		{name: "partial hour attribution", usageRows: 2, landings: 2, partialHour: true, populationObserved: 2, costPerShipped: 4, tokensPerShipped: 120},
		{name: "usage population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: 2, clipped: true, populationObserved: 1000, costPerShipped: 2000, tokensPerShipped: 60000},
		{name: "partial hour population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: 2, partialHour: true, clipped: true, populationObserved: 1000, costPerShipped: 2000, tokensPerShipped: 60000},
		{name: "exclusive end does not clip population", usageRows: maxAnalyticsPopulation + 1, landings: 2, usageAtWindowEnd: true, populationObserved: maxAnalyticsPopulation, costPerShipped: 2000, tokensPerShipped: 60000},
		{name: "landing population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: maxAnalyticsPopulation + 1, clipped: true, populationObserved: 1000, costPerShipped: 4, tokensPerShipped: 120},
		{name: "missing recorded usage", landings: 2},
		{name: "no shipped outcomes", usageRows: 2, populationObserved: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
			f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.Clock = func() time.Time { return now }; cfg.RunDetails = &memoryRunDetails{} })
			user := f.user(t, "analytics", "member", "analytics@example.test", "read", "")
			issue := f.seedIssue(t, 1)
			w := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Hour), To: now, Bucket: time.Hour}
			if test.partialHour {
				w.From = w.From.Add(15 * time.Minute)
				w.To = w.To.Add(15 * time.Minute)
			}
			stamp := now.Add(-time.Hour)
			scope := hubscope.NativeScope{Organization: "org_security", Project: f.project}
			db := hostedPostgresAdmin(t, f.service)
			attempts := map[string]bool{}
			for i := range test.usageRows {
				period := stamp
				if test.usageAtWindowEnd && i == test.usageRows-1 {
					period = w.To
				}
				attemptID := fmt.Sprintf("attempt-%04d", i)
				if test.sharedAttempt {
					attemptID = "shared-attempt"
				}
				if !attempts[attemptID] {
					started, recorded := stamp.Add(-time.Minute), stamp
					if test.usageAtWindowEnd && i == test.usageRows-1 {
						started, recorded = w.To, w.To.Add(time.Minute)
					}
					seedAnalyticsTestAttempt(t, db, f.service, scope, issue, attemptID, "succeeded", started, recorded, tracker.NativeRunData{})
					attempts[attemptID] = true
				}
				if err := f.service.config.TenantRepository.(*postgres.Repository).ImportReportUsageFact(t.Context(), reportstore.UsageRecord{OrganizationID: scope.Organization, UpdatedAt: stamp, UsageFact: reportstore.UsageFact{ProjectID: scope.Project, WorkItemID: issue, AttemptID: attemptID, Period: period, Provider: "openai", Model: fmt.Sprintf("model-%04d", i), Input: 100, Output: 20, Cost: 4, Currency: "USD"}}); err != nil {
					t.Fatal(err)
				}
			}
			for i := range test.landings {
				seedAnalyticsTestChange(t, f.service, scope, issue, fmt.Sprintf("change-%04d", i), stamp, &tracker.ChangeLanding{MergeSHA: strings.Repeat("e", 40), BaseRef: "develop", LandedAt: stamp})
			}
			ctx, credential := analyticsHostedTestAuthority(t, f, user)
			report, err := f.service.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: string(f.project), Limit: 1}, w)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Projects) != 1 {
				t.Fatalf("projects = %#v", report.Projects)
			}
			project := report.Projects[0]
			if project.PopulationLimit != 1000 || (test.clipped && !project.Partial) {
				t.Fatalf("population contract: %+v", project)
			}
			if test.usageRows > 1000 && !test.usageAtWindowEnd {
				cursor := project.PopulationCursors["usage"]
				if cursor == "" {
					t.Fatal("missing continuation cursor")
				}
				next, err := f.service.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{ProjectID: string(f.project), Limit: 1, PopulationCursor: cursor}, w)
				if err != nil {
					t.Fatal(err)
				}
				tail := next.Projects[0]
				if tail.UsageRowsObserved != 1 || !tail.Partial || !tail.CostPerOutcome.Clipped || tail.PopulationCursors["usage"] != "" {
					t.Fatalf("continuation: %+v", tail)
				}
			}
			outcome := project.CostPerOutcome
			if outcome.Shipped != min(test.landings, maxAnalyticsPopulation) || outcome.PopulationObserved != test.populationObserved || outcome.Clipped != test.clipped {
				t.Fatalf("outcome coverage = %#v", outcome)
			}
			if test.clipped {
				if outcome.PopulationTotal != nil {
					t.Fatalf("clipped population total = %d", *outcome.PopulationTotal)
				}
			} else if outcome.PopulationTotal == nil || *outcome.PopulationTotal != test.populationObserved {
				t.Fatalf("complete population total = %#v", outcome.PopulationTotal)
			}
			if test.usageRows > 0 && test.landings > 0 {
				if outcome.CostPerShipped == nil || *outcome.CostPerShipped != test.costPerShipped || outcome.TokensPerShipped == nil || *outcome.TokensPerShipped != test.tokensPerShipped {
					t.Fatalf("outcome ratios = %#v", outcome)
				}
			} else if outcome.CostPerShipped != nil || outcome.TokensPerShipped != nil {
				t.Fatalf("ratio without usage or outcomes = %#v", outcome)
			}
			if slices.Contains(project.Unavailable, "cost_per_outcome_complete_population") != test.clipped || slices.Contains(project.Unavailable, "usage_partial_hour_attribution") != test.partialHour || slices.Contains(project.Unavailable, "recorded_usage") != (test.usageRows == 0) {
				t.Fatalf("unavailable = %v", project.Unavailable)
			}
			raw, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), fmt.Sprintf(`"population_observed":%d`, test.populationObserved)) || !strings.Contains(string(raw), fmt.Sprintf(`"clipped":%t`, test.clipped)) || strings.Contains(string(raw), `"population_total"`) == test.clipped {
				t.Fatalf("serialized coverage = %s", raw)
			}
		})
	}
}

func TestNativeAnalyticsRuntimePopulation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 3, 7, 0, 0, time.UTC)
	clock := now.Add(-15 * time.Minute)
	f := newDefaultNativeFixture(t, Config{Clock: func() time.Time { return clock }, RunDetails: &memoryRunDetails{}})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "recorded phase")
	worker := f.worker(t, "analytics")
	scope := analyticsTestScope(t, f)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
	if err != nil {
		t.Fatal(err)
	}
	appendAnalyticsTestEvent(t, f.service, scope, issue.WorkItemID, "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{RunnerID: credential.Runner.RunnerID, Source: "native_provider_capacity", Outcome: "skipped", Reason: "Current provider requirement has no available reservation", At: clock, WorkItemRevision: issue.Revision}}, clock)
	clock = now.Add(-10 * time.Minute)
	lease := claimNativeAttempt(t, f, worker, "analytics-machine", "analytics-session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	start.Data.Identity.Role = "merge"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 900}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
	clock, start.Type, start.IdempotencyKey, start.Data.Sequence = now, "run.observed", "analytics-runtime", 2
	start.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "planning", StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-5 * time.Minute)}}, LocalAttemptID: 1, Generation: 1}
	commandTiming := (gate.CommandResult{Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-3 * time.Minute), ExitCode: 1}).PipelineTiming("worker_check_land")
	start.Data.Runtime.Pipeline = []gate.PipelineTiming{commandTiming, gate.Interval("rebase", now.Add(-2*time.Minute), now.Add(-time.Minute), "conflict")}
	start.Data.Runtime.Identity = agentidentity.RuntimeUpdate("merge-model", "openai", "high", "", now)
	start.Data.Runtime.Identity.Role, start.Data.Runtime.Identity.BackendKind = "merge", "codex"
	start.Data.Runtime.Landing = &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: now}
	start.Data.Usage = []tracker.NativeUsage{{Provider: "openai", Model: "merge-model", Input: 10}}
	start.Data.Runtime.Activity = &workflowmetrics.ActivityProfile{
		Schema: 1, AttemptID: 1, Generation: 1,
		StartedAt: now.Add(-10 * time.Minute), AsOf: now.Add(-5 * time.Minute), FinishedAt: now.Add(-5 * time.Minute),
		Status: "completed", Dropped: 2,
		Sources: []workflowmetrics.InstructionRef{{Name: "AGENTS.md (effective)", Hash: strings.Repeat("a", 64), ObservedAt: now.Add(-10 * time.Minute)}},
		Spans: []workflowmetrics.ActivitySpan{
			{ID: "context", Kind: "context_read", StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-9 * time.Minute), Outcome: "completed"},
			{ID: "edit", Kind: "implementation", StartedAt: now.Add(-9 * time.Minute), FinishedAt: now.Add(-7 * time.Minute), Outcome: "completed"},
			{ID: "wait", Kind: "waiting", StartedAt: now.Add(-8 * time.Minute), FinishedAt: now.Add(-6 * time.Minute), Outcome: "completed"},
		},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start), http.StatusOK)
	w := operatortool.AnalyticsWindow{From: now.Truncate(time.Hour).Add(-time.Hour), To: now.Truncate(time.Hour).Add(time.Hour), Bucket: time.Hour}
	report, err := f.service.readNativeAnalyticsTenant(t.Context(), scope, operatortool.AnalyticsRequest{Limit: 1}, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Pipeline.Stages) != 2 || len(report.PipelineReceipts.Items) != 1 || report.PipelineReceipts.NextOffset == nil || *report.PipelineReceipts.NextOffset != 1 || report.Pipeline.Stages[1].Duration.Seconds != 60 || report.Pipeline.Stages[1].Executed != 1 {
		t.Fatalf("persisted pipeline timing or pagination missing: %+v", report.Pipeline)
	}
	if report.CostPerOutcome.Shipped != 0 || len(report.Efficiency) != 1 || report.Efficiency[0].Seconds != 300 || len(report.Attempts.Items) != 1 {
		t.Fatalf("runtime %#v", report)
	}
	attempt := report.Attempts.Items[0]
	if attempt.Identity.Role != "merge" || attempt.Identity.ResolvedModel.Value != "merge-model" || attempt.Landing == nil || attempt.Landing.Refusal != "conflict" || attempt.UsageRowsObserved != 1 {
		t.Fatalf("merge analytics = %#v", attempt)
	}
	if report.ProviderCapacityWait.IntervalsObserved != 1 || report.ProviderCapacityWait.Seconds < 300 || report.ProviderCapacityWait.Seconds > 305 || !report.ProviderCapacityWait.Partial {
		t.Fatalf("recorded queue %#v", report.ProviderCapacityWait)
	}
	if report.Activity.ProfilesObserved != 1 || report.Activity.Timing.ElapsedSeconds != 300 || report.Activity.Timing.ObservedSeconds != 240 || report.Activity.Timing.UnknownSeconds != 60 || report.Activity.Timing.ConcurrentSeconds != 60 || report.Activity.DroppedEvents != 2 {
		t.Fatalf("activity %#v", report.Activity)
	}
	if report.Digest[0].Activity.Timing.ObservedSeconds != 180 || report.Digest[0].Activity.Timing.ConcurrentSeconds != 60 || report.Digest[1].Activity.Timing.ObservedSeconds != 60 || report.Digest[1].Activity.Timing.UnknownSeconds != 60 {
		t.Fatalf("cross-hour detailed activity %#v", report.Digest)
	}
	receipt := report.Attempts.Items[0].Activity
	if receipt == nil || len(receipt.Spans) != 0 || receipt.Summary == nil || len(receipt.Sources) != 1 || receipt.Sources[0].Hash != strings.Repeat("a", 64) || !strings.Contains(strings.Join(report.Unavailable, ","), "private_instruction_causality") {
		t.Fatalf("bounded activity receipt %#v", receipt)
	}
	if len(receipt.Summary.Hourly) != 2 {
		t.Fatalf("hourly receipt missing: %+v", receipt.Summary)
	}
	for _, test := range []struct {
		name                                       string
		profile                                    *workflowmetrics.ActivityProfile
		from, to                                   time.Time
		observed, unknown, concurrent, unallocated float64
		unavailable                                int
		malformedPhases                            bool
		bucket                                     time.Duration
	}{
		{name: "hourly totals survive omitted spans", profile: receipt, from: w.From, to: w.To, observed: 240, unknown: 60, concurrent: 60, bucket: time.Hour},
		{name: "malformed phases preserve activity", profile: start.Data.Runtime.Activity, from: w.From, to: w.To, observed: 240, unknown: 60, concurrent: 60, malformedPhases: true},
		{name: "clipped detailed intervals", profile: start.Data.Runtime.Activity, from: now.Add(-9*time.Minute - 30*time.Second), to: now.Add(-6*time.Minute - 30*time.Second), observed: 180, concurrent: 60},
		{name: "missing receipt", from: w.From, to: w.To, unavailable: 1},
		{name: "malformed timestamps", profile: &workflowmetrics.ActivityProfile{Schema: 1}, from: w.From, to: w.To, unavailable: 1},
		{name: "compacted whole attempt", profile: func() *workflowmetrics.ActivityProfile {
			p := *receipt
			p.DetailOmitted = 3
			p.Summary = &workflowmetrics.ActivitySummary{Through: p.Summary.Through, DetailFrom: p.Summary.Through, Breakdown: p.Summary.Breakdown}
			return &p
		}(), from: w.From, to: w.To, observed: 240, unknown: 60, concurrent: 60},
		{name: "compacted partial window", profile: func() *workflowmetrics.ActivityProfile {
			p := *receipt
			p.DetailOmitted = 3
			p.Summary = &workflowmetrics.ActivitySummary{Through: p.Summary.Through, DetailFrom: p.Summary.Through, Breakdown: p.Summary.Breakdown}
			return &p
		}(), from: now.Add(-9 * time.Minute), to: now.Add(-7 * time.Minute), unallocated: 120},
		{name: "compacted prefix with retained tail", profile: func() *workflowmetrics.ActivityProfile {
			p := *receipt
			p.DetailOmitted = 2
			p.ProjectionOmitted = 1
			p.Unpaired = 1
			p.Summary = &workflowmetrics.ActivitySummary{Through: p.Summary.Through, DetailFrom: now.Add(-7 * time.Minute), Breakdown: p.Summary.Breakdown}
			p.Spans = []workflowmetrics.ActivitySpan{start.Data.Runtime.Activity.Spans[2]}
			return &p
		}(), from: now.Add(-9 * time.Minute), to: now.Add(-5 * time.Minute), observed: 60, unknown: 60, unallocated: 120},
		{name: "malformed summary", profile: func() *workflowmetrics.ActivityProfile {
			p := *receipt
			p.Summary = &workflowmetrics.ActivitySummary{Through: p.AsOf, DetailFrom: p.StartedAt}
			return &p
		}(), from: w.From, to: w.To, unavailable: 1},
		{name: "malformed hourly summary", profile: func() *workflowmetrics.ActivityProfile {
			p := *receipt
			summary := *p.Summary
			summary.Hourly = append([]workflowmetrics.ActivityHour(nil), summary.Hourly...)
			summary.Hourly[0].Breakdown.ElapsedSeconds++
			p.Summary = &summary
			return &p
		}(), from: w.From, to: w.To, unavailable: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := *start.Data.Runtime
			runtime.Activity = test.profile
			fixture := newNativeFixture(t, f.service, f.project.OrganizationID, "activity "+test.name)
			caseScope := analyticsTestScope(t, fixture)
			caseItem := seedAnalyticsTestWorkItem(t, f.service, caseScope.Organization, caseScope.Project, "activity-item", 1, test.name, "Todo", now.Add(-15*time.Minute))
			seedAnalyticsTestAttempt(t, hostedPostgresAdmin(t, f.service), f.service, caseScope, caseItem.WorkItemID, "activity-"+string(caseScope.Project), "succeeded", now.Add(-10*time.Minute), now, tracker.NativeRunData{Runtime: &runtime}, test.malformedPhases)
			bucket := test.bucket
			if bucket == 0 {
				bucket = time.Minute
			}
			window := operatortool.AnalyticsWindow{From: test.from, To: test.to, Bucket: bucket}
			got, err := f.service.readNativeAnalyticsTenant(t.Context(), caseScope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			if test.malformedPhases && (!got.Partial || len(got.Efficiency) != 0 || !slices.Contains(got.Unavailable, "recorded_phases_malformed")) {
				t.Fatalf("phase boundary %#v", got)
			}
			activity := got.Activity
			if activity.Timing.ObservedSeconds != test.observed || activity.Timing.UnknownSeconds != test.unknown || activity.Timing.ConcurrentSeconds != test.concurrent || activity.UnallocatedSeconds != test.unallocated || activity.ProfilesUnavailable != test.unavailable {
				t.Fatalf("window activity %#v", activity)
			}
			if test.bucket == time.Hour && (got.Digest[0].Activity.ProfilesObserved != 1 || got.Digest[1].Activity.ProfilesObserved != 1 || got.Digest[0].Activity.Timing.ObservedSeconds != 180 || got.Digest[1].Activity.Timing.ObservedSeconds != 60 || got.Digest[1].Activity.Timing.UnknownSeconds != 60) {
				t.Fatalf("receipt not attributed to both hours: %+v", got.Digest)
			}
			var observed, unknown, concurrent, unallocated float64
			for _, b := range got.Digest {
				observed += b.Activity.Timing.ObservedSeconds
				unknown += b.Activity.Timing.UnknownSeconds
				concurrent += b.Activity.Timing.ConcurrentSeconds
				unallocated += b.Activity.UnallocatedSeconds
			}
			if test.name == "compacted whole attempt" {
				if observed != 0 || unallocated != 300 {
					t.Fatalf("compacted hour allocation: observed=%v unallocated=%v", observed, unallocated)
				}
			} else if observed != test.observed || unknown != test.unknown || concurrent != test.concurrent || unallocated != test.unallocated {
				t.Fatalf("bucket allocation: %v %v %v %v; receipt=%+v", observed, unknown, concurrent, unallocated, test.profile.Summary)
			}
		})
	}
	for range maxAnalyticsPopulation + 1 {
		appendAnalyticsTestEvent(t, f.service, scope, issue.WorkItemID, "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "recorded", Outcome: "skipped", Reason: "capacity", At: now}}, now)
	}
	report, err = f.service.readNativeAnalyticsTenant(t.Context(), scope, operatortool.AnalyticsRequest{Limit: 1}, w)
	if err != nil {
		t.Fatal(err)
	}
	if report.DecisionsObserved != 1000 || len(report.SkipReasons) != 2 || report.SkipReasons[1].Count != 999 {
		t.Fatalf("bounded history %#v", report)
	}
	for _, test := range []struct {
		name        string
		source      string
		runner      string
		revision    tracker.Revision
		interrupted bool
		seconds     float64
	}{
		{name: "capacity wait clipped to window", source: "native_provider_capacity", revision: 1, seconds: 60},
		{name: "host capacity precedes eligibility", source: "native_host_capacity", revision: 1},
		{name: "different runner", source: "native_provider_capacity", runner: "other", revision: 1},
		{name: "changed item revision", source: "native_provider_capacity", revision: 2},
		{name: "operator hold intervenes", source: "native_provider_capacity", revision: 1, interrupted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNativeFixture(t, f.service, f.project.OrganizationID, test.name)
			caseScope := analyticsTestScope(t, fixture)
			item := seedAnalyticsTestWorkItem(t, f.service, caseScope.Organization, caseScope.Project, "capacity-item", 1, test.name, "Todo", now.Add(-15*time.Minute))
			priorAt, claimedAt := now.Add(-4*time.Minute), now.Add(-time.Minute)
			prior := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: test.source, Outcome: "skipped", RunnerID: test.runner, WorkItemRevision: test.revision, At: priorAt}}
			claim := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", WorkItemRevision: 1, At: claimedAt}}
			appendAnalyticsTestEvent(t, f.service, caseScope, item.WorkItemID, "scheduler.decision", prior, priorAt)
			if test.interrupted {
				appendAnalyticsTestEvent(t, f.service, caseScope, item.WorkItemID, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Human Review"}, now.Add(-3*time.Minute))
			}
			appendAnalyticsTestEvent(t, f.service, caseScope, item.WorkItemID, "scheduler.decision", claim, claimedAt)
			window := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Minute), To: now, Bucket: time.Minute}
			got, err := f.service.readNativeAnalyticsTenant(t.Context(), caseScope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			if got.ProviderCapacityWait.ClaimsObserved != 1 || got.ProviderCapacityWait.Seconds != test.seconds || got.Digest[0].ProviderCapacityWaitSeconds != test.seconds || got.Digest[1].ProviderCapacityWaitSeconds != 0 || (got.ProviderCapacityWait.IntervalsObserved == 1) != (test.seconds > 0) {
				t.Fatalf("queue boundary %#v", got.ProviderCapacityWait)
			}
		})
	}
	foreign := scope
	foreign.Project = "prj_foreign"
	hidden, err := f.service.readNativeAnalyticsTenant(t.Context(), foreign, operatortool.AnalyticsRequest{Limit: 1}, w)
	if !hubapi.IsNativeNotFound(err) {
		t.Fatalf("foreign project authorization error=%v", err)
	}
	if hidden.AttemptsObserved != 0 || hidden.DecisionsObserved != 0 || hidden.CostPerOutcome.Shipped != 0 {
		t.Fatalf("foreign population %#v", hidden)
	}
}

func seedAnalyticsTestWorkItem(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID, id string, number int, title, state string, at time.Time) tracker.NativeIssue {
	t.Helper()
	issue, create, created := prepareAnalyticsTestWorkItem(t, service, organization, project, id, number, title, state, at)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := create(w); err != nil {
			return err
		}
		return created(w)
	}); err != nil {
		t.Fatal(err)
	}
	return issue
}

func prepareAnalyticsTestWorkItem(t *testing.T, service *Service, organization tracker.OrganizationID, project tracker.ProjectID, id string, number int, title, state string, at time.Time) (tracker.NativeIssue, func(tenantstore.Writer) error, func(tenantstore.Writer) error) {
	t.Helper()
	id = string(project) + "-" + id
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(id), Number: number, Revision: 1}, Title: title, Body: nativeContractTestBody, State: state, Type: "bug", Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: at, UpdatedAt: at, LastActivityAt: at}
	fields, err := service.prepareWorkItemFields(t.Context(), organization, hubrecords.WorkItemRecordPlan{Issue: issue, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(service.runDetailStore())
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: issue.WorkItemID, RecordID: id, Revision: 1}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), organization, historyTestJSON(t, tracker.CollaborationEvent{ID: "created-" + id, OrganizationID: organization, ProjectID: project, AggregateID: issue.WorkItemID, Type: "issue.created", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: at, Data: tracker.CollaborationData{Revision: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	return issue, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: issue.WorkItemID, Number: int64(number), Revision: 1, Title: title, IssueType: issue.Type, BodyObjectKey: &fields.Body, IssueContractObjectKey: fields.Contract, WorkflowStateName: &state, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID, CreatedAt: at, UpdatedAt: at, NativeCreatedAt: at, NativeUpdatedAt: at, LastActivityAt: at}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}, func(w tenantstore.Writer) error {
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}
}

func analyticsTestScope(t *testing.T, f nativeFixture) hubscope.NativeScope {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("analytics scope status=%d error=%v", status, err)
	}
	return hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
}

func seedAnalyticsTestChange(t *testing.T, service *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, at time.Time, landing *tracker.ChangeLanding) (tracker.ChangeRequest, tracker.ChangeVersion) {
	t.Helper()
	actor := tracker.Actor{Kind: "human", PrincipalID: "example"}
	version := tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("b", 40), PolicyID: "example-policy"}, ID: "version-" + id, ChangeID: id, Number: 1, Actor: actor, CreatedAt: at}
	change := tracker.ChangeRequest{ID: id, OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, Revision: 1, Title: id, CurrentVersion: version.ID, CreatedAt: at, UpdatedAt: at}
	if landing != nil {
		copy := *landing
		copy.VersionID, copy.HeadSHA = version.ID, version.HeadSHA
		change.Landed = &copy
	}
	content := eventstore.NewContent(service.runDetailStore())
	changeRow, err := content.PrepareChange(t.Context(), scope.Organization, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	versionRow, err := content.PrepareVersion(t.Context(), scope.Organization, historyTestJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	var frozen eventstore.VersionLanding
	if change.Landed != nil {
		frozen, err = content.PrepareVersionLanding(t.Context(), eventstore.VersionLanding{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, ChangeID: id, VersionID: version.ID}, *change.Landed)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
			return err
		}
		if change.Landed != nil {
			return w.RecordVersionLanding(t.Context(), frozen)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return change, version
}

func analyticsHostedTestAuthority(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) (context.Context, hubapi.APICredential) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(t.Context())
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
	c := f.service.echo.NewContext(request, httptest.NewRecorder())
	c.SetPath("/mcp")
	var ctx context.Context
	if err := f.service.operatorAuthority(func(c echo.Context) error { ctx = c.Request().Context(); return nil })(c); err != nil || ctx == nil {
		t.Fatalf("analytics authority: %v", err)
	}
	credential, err := currentHubOperator(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, credential
}

func seedAnalyticsTestAttempt(t *testing.T, db *sql.DB, service *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id, status string, started, recorded time.Time, data tracker.NativeRunData, malformedPhases ...bool) eventstore.Event {
	t.Helper()
	data.AttemptID, data.RunID, data.Sequence, data.Outcome = id, "run-"+id, 1, status
	eventType := "run.finished"
	var releasedAt any = recorded
	if status == "running" {
		eventType, data.Outcome, releasedAt = "run.started", "", nil
	}
	historicalMalformed := len(malformedPhases) > 0 && malformedPhases[0]
	if historicalMalformed {
		runtime := tracker.NativeRuntimeObservation{}
		if data.Runtime != nil {
			runtime = *data.Runtime
		}
		runtime.Phases = nil
		data.Runtime = &runtime
	}
	content := eventstore.NewContent(service.runDetailStore())
	event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: "event-" + id, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, Type: eventType, SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "analytics-fixture"}, RecordedAt: recorded, Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	if historicalMalformed {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(historyTestJSON(t, tracker.CollaborationEvent{ID: event.ID, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, Type: event.Type, SchemaVersion: 1, Actor: tracker.Actor{Kind: event.ActorKind, PrincipalID: event.ActorPrincipalID}, RecordedAt: recorded, Data: tracker.CollaborationData{Run: &data}}), &envelope); err != nil {
			t.Fatal(err)
		}
		var fields, run, runtime map[string]json.RawMessage
		if err := json.Unmarshal(envelope["data"], &fields); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(fields["run"], &run); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(run["runtime"], &runtime); err != nil {
			t.Fatal(err)
		}
		runtime["phases"] = json.RawMessage(`"malformed"`)
		run["runtime"] = historyTestJSON(t, runtime)
		fields["run"] = historyTestJSON(t, run)
		envelope["data"] = historyTestJSON(t, fields)
		ref, err := content.PreparePayload(t.Context(), scope.Organization, historyTestJSON(t, envelope))
		if err != nil {
			t.Fatal(err)
		}
		event.PayloadObjectKey, event.PayloadSHA256, event.PayloadBytes, event.PayloadContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
	}
	var issueID int64
	if err := db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", scope.Organization, scope.Project, item).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO machines(organization_id,id,hostname,version,capacity,last_heartbeat_at,registered_at,updated_at) VALUES($1,'analytics-fixture','example','example',1,$2,$2,$2) ON CONFLICT(organization_id,id) DO NOTHING`, scope.Organization, started); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,$2,$3)", scope.Organization, data.RunID, issueID); err != nil {
		t.Fatal(err)
	}
	var fence int64
	if err := db.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) VALUES($1,$2,$3,'analytics-fixture',$4,1,$5,$5,$6,$7) RETURNING fencing_token`, scope.Organization, "lease-"+id, issueID, "session-"+id, started, recorded.Add(time.Hour), releasedAt).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,1,1,$9,$10,$11,$12)`, scope.Organization, scope.Project, id, issueID, "lease-"+id, fence, data.RunID, status, event.PayloadObjectKey, started.Truncate(time.Microsecond), started.Nanosecond()%1000, recorded); err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
		t.Fatal(err)
	}
	return event
}

func appendAnalyticsTestEvent(t *testing.T, service *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, kind string, data tracker.CollaborationData, at time.Time) eventstore.Event {
	t.Helper()
	row, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: at, Data: data}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		row, err = w.AppendEvent(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return row
}

func transitionAnalyticsTestWorkItem(t *testing.T, service *Service, scope hubscope.NativeScope, item tracker.NativeWorkItemID, target string, at time.Time) {
	t.Helper()
	var row corestore.WorkItem
	var version eventstore.CollaborationVersion
	if err := service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.WorkItem(t.Context(), scope.Project, item)
		if err != nil {
			return err
		}
		version, err = r.CollaborationVersion(t.Context(), string(item), row.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	issue, err := service.hydrateCanonicalWorkItemRecord(t.Context(), row, version)
	if err != nil {
		t.Fatal(err)
	}
	from := issue.State
	issue.State = target
	issue.Revision++
	issue.UpdatedAt = at
	content := eventstore.NewContent(service.runDetailStore())
	version, err = content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: item, RecordID: string(item), Revision: int64(issue.Revision)}, historyTestJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), scope.Organization, historyTestJSON(t, tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: at, Data: tracker.CollaborationData{Revision: issue.Revision, FromState: from, ToState: target}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(t.Context(), scope.Project); err != nil {
			return err
		}
		row.WorkflowStateName = &target
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		_, err := w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type analyticsReadAcquireKey struct{}
type analyticsReadAcquireGuard struct {
	nested atomic.Bool
	cancel context.CancelFunc
}

type analyticsReadPoolTrace struct{}

func (analyticsReadPoolTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (analyticsReadPoolTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (analyticsReadPoolTrace) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	guard, _ := ctx.Value(analyticsReadAcquireKey{}).(*analyticsReadAcquireGuard)
	if guard != nil && pool.Stat().AcquiredConns() != 0 {
		guard.nested.Store(true)
		guard.cancel()
	}
	return ctx
}
func (analyticsReadPoolTrace) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}
