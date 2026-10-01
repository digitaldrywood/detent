package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/web"
)

// Catches budget effects before exact approval, replay changing the allowance
// lifetime, YOLO bypassing configured limits/grants, and leaking usage totals.
func TestMCPDaemonBilling(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"approve", "clear", "stale allowance", "replay", "changed replay", "read scope", "foreign project", "YOLO limits", "usage", "explanation"} {
		t.Run(scenario, func(t *testing.T) {
			deps := testDeps(t)
			deps.Store = openWebTestStore(t)
			if err := deps.Registry.Set(newBudgetTestProject(t, "detent", 100, 10)); err != nil {
				t.Fatal(err)
			}
			deps.IssueExplainer = &fakeIssueExplainer{result: explain.IssueExplanation{Schema: explain.SchemaVersion, Found: true, ObservedAt: time.Now().UTC(), Identity: explain.Identity{ProjectID: "detent", IssueID: "issue"}}}
			server, err := web.NewServer(web.Config{ServerAddress: "127.0.0.1:0", GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "human-only-fixture", AllowWrite: true}}}, deps)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Shutdown(context.WithoutCancel(t.Context())) })
			scopes, projects := []string{"write"}, []string{"detent"}
			if scenario == "read scope" {
				scopes = []string{"read"}
			}
			if scenario == "foreign project" {
				projects = []string{"other"}
			}
			token, _ := createRemoteMCPKey(t, server, "Billing fixture", scopes, projects)
			headers := map[string]string{"Authorization": "Bearer " + token}
			setup := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, headers)
			var connection struct {
				ID string `json:"connection_id"`
			}
			if json.Unmarshal(setup.Body.Bytes(), &connection) != nil || connection.ID == "" {
				t.Fatalf("setup=%s", setup.Body.String())
			}
			headers["X-Detent-Connection-ID"] = connection.ID
			call := func(name, args string) *httptest.ResponseRecorder {
				return performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, args, headers)
			}
			entry := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/?token=human-only-fixture"})
			cookies := entry.Result().Cookies()
			decision := func(actionID, decision, mode string) {
				view := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + connection.ID, cookies: cookies})
				if view.Code == http.StatusSeeOther {
					cookies = append(cookies, view.Result().Cookies()...)
					view = performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + connection.ID, cookies: cookies})
				}
				cookies = append(cookies, view.Result().Cookies()...)
				html := view.Body.String()
				if actionID != "" {
					for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
						if strings.Contains(form, `name="action_id" value="`+actionID+`"`) {
							html = form
							break
						}
					}
				}
				match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(html)
				if len(match) != 2 {
					t.Fatalf("approval page=%d %s", view.Code, html)
				}
				form := url.Values{"connection_id": {connection.ID}, "action_id": {actionID}, "decision": {decision}, "mode": {mode}, "form_token": {match[1]}}
				response := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/chat/approval", cookies: cookies, form: form})
				if scenario == "stale allowance" && decision == "confirm" && response.Code == http.StatusOK && strings.Contains(response.Body.String(), `role="alert"`) {
					return
				}
				if response.Code != http.StatusSeeOther {
					t.Fatalf("decision=%d %s", response.Code, response.Body.String())
				}
			}
			if scenario == "usage" {
				for _, id := range []string{"detent", "other"} {
					if _, err := deps.Store.RecordUsageEvent(t.Context(), store.UsageEvent{ProjectID: id, IssueID: "issue", Model: "fixture", TotalTokens: 10, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), Outcome: "completed"}); err != nil {
						t.Fatal(err)
					}
				}
				response := call(operatortool.UsageReport, `{}`)
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"total_tokens":10`) || strings.Contains(response.Body.String(), `"total_tokens":20`) {
					t.Fatalf("usage=%s", response.Body.String())
				}
				return
			}
			if scenario == "explanation" {
				response := call(operatortool.IssueExplanation, `{"project_id":"detent","reference":"issue"}`)
				if response.Code != 200 || !strings.Contains(response.Body.String(), `"found":true`) {
					t.Fatalf("explanation=%s", response.Body.String())
				}
				if denied := call(operatortool.IssueExplanation, `{"project_id":"other","reference":"issue"}`); denied.Code == 200 {
					t.Fatal("foreign explanation allowed")
				}
				return
			}
			writer := deps.Store.(store.BudgetOverrideStore)
			if scenario == "YOLO limits" {
				decision("", "mode", "yolo")
			}
			args := `{"project_id":"detent","per_day_max_usd":200,"duration":"4h","reason":"release work","request_id":"override"}`
			if scenario == "YOLO limits" {
				args = strings.Replace(args, `:200`, `:1000000`, 1)
			}
			response := call(operatortool.BudgetOverrideSet, args)
			if scenario == "read scope" || scenario == "foreign project" || scenario == "YOLO limits" {
				if response.Code == 200 {
					t.Fatalf("denied mutation=%s", response.Body.String())
				}
				if _, err := writer.ActiveBudgetOverride(t.Context(), "detent", time.Now()); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("denied allowance changed: %v", err)
				}
				return
			}
			var preview struct {
				Preview chat.Action `json:"preview"`
			}
			if json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.Preview.Status != chat.ActionPending {
				t.Fatalf("preview=%s", response.Body.String())
			}
			if _, err := writer.ActiveBudgetOverride(t.Context(), "detent", time.Now()); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("budget written before approval")
			}
			if scenario == "stale allowance" {
				form := url.Values{"per_day_max_usd": {"150"}, "duration": {"1h"}, "reason": {"operator change"}}
				performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/api/v1/projects/detent/budget/override", headers: map[string]string{"Authorization": "Bearer detent_admin_token"}, form: form})
			}
			decision(preview.Preview.ID, "confirm", "")
			active, err := writer.ActiveBudgetOverride(t.Context(), "detent", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "stale allowance" {
				if active.PerDayMaxUSD == nil || *active.PerDayMaxUSD != 150 {
					t.Fatalf("stale preview overwrote allowance=%+v", active)
				}
				return
			}
			if active.PerDayMaxUSD == nil || *active.PerDayMaxUSD != 200 {
				t.Fatalf("approved budget=%+v", active)
			}
			if scenario == "clear" {
				response = call(operatortool.BudgetOverrideClear, `{"project_id":"detent","request_id":"clear"}`)
				if json.Unmarshal(response.Body.Bytes(), &preview) != nil || preview.Preview.Status != chat.ActionSucceeded {
					t.Fatalf("ordinary clear=%s", response.Body.String())
				}
				if _, err := writer.ActiveBudgetOverride(t.Context(), "detent", time.Now()); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("clear failed=%v", err)
				}
			}
			if scenario == "replay" || scenario == "changed replay" {
				if scenario == "changed replay" {
					args = strings.Replace(args, `:200`, `:250`, 1)
				}
				retry := call(operatortool.BudgetOverrideSet, args)
				if (retry.Code == 200) != (scenario == "replay") {
					t.Fatalf("retry=%d %s", retry.Code, retry.Body.String())
				}
				after, err := writer.ActiveBudgetOverride(t.Context(), "detent", time.Now())
				if err != nil || !after.ExpiresAt.Equal(active.ExpiresAt) {
					t.Fatal("replay extended allowance lifetime")
				}
			}
		})
	}
}
