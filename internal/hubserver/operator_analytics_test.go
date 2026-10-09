package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestHostedAnalyticsReads(t *testing.T) {
	now := time.Now().UTC()
	f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.now = func() time.Time { return now } })
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled)
SELECT 'prj_other',organization_id,'foreign-project-sentinel','github_compatible',states_json,created_at,github_repository_enabled FROM projects WHERE id=?`, f.project); err != nil {
		t.Fatal(err)
	}
	other := f
	other.project = "prj_other"
	scope := nativeScope{organization: "org_security", project: f.project}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "In Progress", Dispatchable: true}, {Name: "Done", Terminal: true}}
	if err := applyNativeProjectStates(t.Context(), tx, scope, states, now); err != nil {
		t.Fatal(err)
	}
	issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "lane residence", State: "Todo"}, now.Add(-6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	issue.State = "In Progress"
	if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
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
				if grant == "both" {
					want = 2
				}
				if len(board.Projects) != want || grant == "native" && strings.Contains(string(raw), "prj_other") || grant == "github" && strings.Contains(string(raw), string(f.project)) {
					t.Fatalf("hosted board project authority: %s", raw)
				}
			})
		}
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
				if report.Requests != nil {
					t.Fatalf("member received tenant-wide traffic: %s", raw)
				}
				want := 1
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
				for _, args := range []map[string]any{{"limit": 201}, {"window": "25h"}, {"from": "bad"}, {"bucket": "1ns"}} {
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
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=?", user.identity.Subject); err != nil {
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
	m.record(now.Add(-time.Minute), "GET /items/:item", "runner", "runner_report", 200, 10*time.Millisecond)
	if err := m.flush(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	f.grant(t, owner, false, false)
	response, raw := call(owner, operatortool.Reports, map[string]any{})
	var report nativeAnalyticsReport
	if err := json.Unmarshal(raw, &report); err != nil || response.Code != http.StatusOK || report.Requests == nil || len(report.Requests.Callers) != 1 || report.Requests.Callers[0].Caller != "runner_report" {
		t.Fatalf("admin MCP traffic: %s err=%v", response.Body.String(), err)
	}
}

func TestNativeAnalyticsUnavailableServices(t *testing.T) {
	identity := operatortool.Identity{PrincipalID: "test", OrganizationID: "org_test", CredentialID: "test"}
	ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
		return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
	}})
	ctx = context.WithValue(ctx, hubOperatorResolverKey{}, func(context.Context) (apiCredential, error) { return apiCredential{}, nil })
	for _, cfg := range []Config{{now: time.Now}, {now: time.Now, Hosted: &HostedConfig{OrganizationID: "org_test"}}} {
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
		{name: "usage population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: 2, populationObserved: 1001, costPerShipped: 2002, tokensPerShipped: 60060},
		{name: "partial hour population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: 2, partialHour: true, populationObserved: 1001, costPerShipped: 2002, tokensPerShipped: 60060},
		{name: "exclusive end does not clip population", usageRows: maxAnalyticsPopulation + 1, landings: 2, usageAtWindowEnd: true, populationObserved: maxAnalyticsPopulation, costPerShipped: 2000, tokensPerShipped: 60000},
		{name: "landing population beyond one page", usageRows: maxAnalyticsPopulation + 1, landings: maxAnalyticsPopulation + 1, populationObserved: 1001, costPerShipped: 4, tokensPerShipped: 120},
		{name: "missing recorded usage", landings: 2},
		{name: "no shipped outcomes", usageRows: 2, populationObserved: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
			f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.now = func() time.Time { return now } })
			user := f.user(t, "analytics", "member", "analytics@example.test", "read", "")
			issue := f.seedIssue(t, 1)
			w := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Hour), To: now, Bucket: time.Hour}
			if test.partialHour {
				w.From = w.From.Add(15 * time.Minute)
				w.To = w.To.Add(15 * time.Minute)
			}
			stamp := now.Add(-time.Hour)
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := range test.usageRows {
				period := stamp
				if test.usageAtWindowEnd && i == test.usageRows-1 {
					period = w.To
				}
				attemptID := fmt.Sprintf("attempt-%04d", i)
				if test.sharedAttempt {
					attemptID = "shared-attempt"
				}
				_, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_usage
(attempt_id,organization_id,project_id,period,provider,model,input,output,cost_estimate,updated_at)
VALUES (?,'org_security',?,?,'openai',?,100,20,4,?)`, attemptID, f.project, usagePeriod(period), fmt.Sprintf("model-%04d", i), formatHubTime(stamp))
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := range test.landings {
				changeID, versionID := fmt.Sprintf("change-%04d", i), fmt.Sprintf("version-%04d", i)
				headSHA := strings.Repeat("b", 40)
				_, err := tx.ExecContext(t.Context(), `INSERT INTO change_requests (id,organization_id,project_id,work_item_id,record_json)
VALUES (?,'org_security',?,?,json_object('landed',json_object('version_id',?,'head_sha',?,'merge_sha',?,'landed_at',?)))`, changeID, f.project, issue, versionID, headSHA, strings.Repeat("e", 40), formatHubTime(stamp))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO change_versions (id,change_id,number,record_json) VALUES (?,?,1,json_object('head_sha',?))`, versionID, changeID, headSHA); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			identity := operatortool.Identity{PrincipalID: user.identity.Subject, OrganizationID: "org_security", CredentialID: "test"}
			ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
			}})
			report, err := f.service.readAnalyticsReport(ctx, apiCredential{Hosted: user.identity.Hosted}, operatortool.AnalyticsRequest{ProjectID: string(f.project), Limit: 1}, w)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Projects) != 1 {
				t.Fatalf("projects = %#v", report.Projects)
			}
			project := report.Projects[0]
			outcome := project.CostPerOutcome
			if outcome.Shipped != test.landings || outcome.PopulationObserved != test.populationObserved || outcome.Clipped != test.clipped {
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
	now := time.Date(2026, 10, 6, 3, 7, 0, 0, time.UTC)
	clock := now.Add(-15 * time.Minute)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return clock }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "recorded phase")
	worker := f.worker(t, "analytics")
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, issueID, err := readNativeIssue(t.Context(), tx, scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	if err := recordNativeSchedulingOutcome(t.Context(), tx, &scope, issueID, tracker.NativeSchedulerDecision{Source: "native_provider_capacity", Outcome: "skipped", Reason: "Current provider requirement has no available reservation"}, clock); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(-10 * time.Minute)
	lease := claimNativeAttempt(t, f, worker, "analytics-machine", "analytics-session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	start.Data.Identity.Role = "merge"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 900}), http.StatusOK)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start)
	requireNativeStatus(t, response, http.StatusOK)
	clock = now
	start.Type, start.IdempotencyKey, start.Data.Sequence = "run.observed", "analytics-runtime", 2
	start.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "planning", StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-5 * time.Minute)}}}
	commandTiming := (gate.CommandResult{Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), StartedAt: now.Add(-4 * time.Minute), FinishedAt: now.Add(-3 * time.Minute), ExitCode: 1}).PipelineTiming("worker_check_land")
	start.Data.Runtime.Pipeline = []gate.PipelineTiming{commandTiming, gate.Interval("rebase", now.Add(-2*time.Minute), now.Add(-time.Minute), "conflict")}
	start.Data.Runtime.LocalAttemptID, start.Data.Runtime.Generation = 1, 1
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
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start)
	requireNativeStatus(t, response, http.StatusOK)
	w := operatortool.AnalyticsWindow{From: now.Truncate(time.Hour).Add(-time.Hour), To: now.Truncate(time.Hour).Add(time.Hour), Bucket: time.Hour}
	report, err := readNativeAnalytics(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 1}, w)
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
		{name: "clipped detailed intervals", profile: start.Data.Runtime.Activity, from: now.Add(-9*time.Minute - 30*time.Second), to: now.Add(-6*time.Minute - 30*time.Second), observed: 180, concurrent: 60},
		{name: "malformed phases preserve activity", profile: start.Data.Runtime.Activity, from: w.From, to: w.To, observed: 240, unknown: 60, concurrent: 60, malformedPhases: true},
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
			raw, err := json.Marshal(runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime',json(?)) WHERE id=?", string(raw), start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if test.malformedPhases {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime.phases',?) WHERE id=?", "malformed", start.Data.AttemptID); err != nil {
					t.Fatal(err)
				}
			}
			bucket := test.bucket
			if bucket == 0 {
				bucket = time.Minute
			}
			window := operatortool.AnalyticsWindow{From: test.from, To: test.to, Bucket: bucket}
			got, err := readNativeAnalytics(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			if test.malformedPhases && (!got.Partial || len(got.Efficiency) != 0 || !strings.Contains(strings.Join(got.Unavailable, ","), "recorded_phases_malformed")) {
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
	raw, err := json.Marshal(start.Data.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime',json(?)) WHERE id=?", string(raw), start.Data.AttemptID); err != nil {
		t.Fatal(err)
	}
	tx, err = f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for range maxAnalyticsPopulation + 1 {

		data := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "recorded", Outcome: "skipped", Reason: "capacity", At: now}}
		if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", data, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	report, err = readNativeAnalytics(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 1}, w)
	if err != nil {
		t.Fatal(err)
	}
	if report.DecisionsObserved != 1002 || len(report.SkipReasons) != 2 || report.SkipReasons[1].Count != 1001 {
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
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			priorAt, claimedAt := now.Add(-4*time.Minute), now.Add(-time.Minute)
			prior := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: test.source, Outcome: "skipped", RunnerID: test.runner, WorkItemRevision: test.revision, At: priorAt}}
			claim := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: "claimed", WorkItemRevision: 1, At: claimedAt}}
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", prior, priorAt); err != nil {
				t.Fatal(err)
			}
			if test.interrupted {
				if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Human Review"}, now.Add(-3*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if err := appendNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "scheduler.decision", claim, claimedAt); err != nil {
				t.Fatal(err)
			}
			window := operatortool.AnalyticsWindow{From: now.Add(-2 * time.Minute), To: now, Bucket: time.Minute}
			got, err := readNativeAnalytics(t.Context(), tx, scope, operatortool.AnalyticsRequest{Limit: 1}, window)
			if err != nil {
				t.Fatal(err)
			}
			if got.ProviderCapacityWait.ClaimsObserved != 1 || got.ProviderCapacityWait.Seconds != test.seconds || got.Digest[0].ProviderCapacityWaitSeconds != test.seconds || got.Digest[1].ProviderCapacityWaitSeconds != 0 || (got.ProviderCapacityWait.IntervalsObserved == 1) != (test.seconds > 0) {
				t.Fatalf("queue boundary %#v", got.ProviderCapacityWait)
			}
		})
	}
	foreign := scope
	foreign.project = "prj_foreign"
	hidden, err := readNativeAnalytics(t.Context(), f.service.database.db, foreign, operatortool.AnalyticsRequest{Limit: 1}, w)
	if err != nil {
		t.Fatal(err)
	}
	if hidden.AttemptsObserved != 0 || hidden.DecisionsObserved != 0 || hidden.CostPerOutcome.Shipped != 0 {
		t.Fatalf("foreign population %#v", hidden)
	}
}
