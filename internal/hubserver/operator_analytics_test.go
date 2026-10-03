package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
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
				want := 1
				if grant == "both" {
					want = 2
				}
				if len(report.Projects) != want || report.ObservedAt.IsZero() || report.OrganizationID != "org_security" {
					t.Fatalf("population %s", raw)
				}
				if grant == "native" && strings.Contains(string(raw), "prj_other") || grant == "github" && strings.Contains(string(raw), string(f.project)) {
					t.Fatalf("cross-project data %s", raw)
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
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
			response, raw := call(user, name, map[string]any{"project_id": string(f.project)})
			if len(raw) > 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
				t.Fatalf("revoked grant %s", response.Body.String())
			}
		}
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

func TestNativeAnalyticsRuntimePopulation(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "recorded phase")
	worker := f.worker(t, "analytics")
	lease := claimNativeAttempt(t, f, worker, "analytics-machine", "analytics-session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	now := f.service.config.now().UTC()
	start.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now, Phases: []tracker.NativePhase{{Name: "planning", StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-5 * time.Minute)}}}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/events", worker, start)
	requireNativeStatus(t, response, http.StatusOK)
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Minute), Bucket: time.Hour}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	report, err := readNativeAnalytics(t.Context(), f.service.database.db, scope, operatortool.AnalyticsRequest{Limit: 1}, w)
	if err != nil {
		t.Fatal(err)
	}
	if report.CostPerOutcome.Shipped != 0 || len(report.Efficiency) != 1 || report.Efficiency[0].Seconds != 300 || len(report.Attempts.Items) != 1 {
		t.Fatalf("runtime %#v", report)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
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
	if !report.Partial || report.DecisionsObserved != maxAnalyticsPopulation || len(report.SkipReasons) != 1 || report.SkipReasons[0].Count != maxAnalyticsPopulation {
		t.Fatalf("bounded history %#v", report)
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
