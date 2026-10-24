package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestMCPAnalyticsReads(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	now := time.Now().UTC()
	for _, id := range []string{"detent", "other"} {
		if err := deps.Registry.Set(newBudgetTestProject(t, id, 100, 10)); err != nil {
			t.Fatal(err)
		}
		if _, err := deps.Store.RecordUsageEvent(t.Context(), store.UsageEvent{ProjectID: id, IssueID: id + "-issue", Model: id + "-model", TotalTokens: 10, StartedAt: now.Add(-time.Hour), FinishedAt: now.Add(-time.Minute), Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		session, err := deps.Store.StartSession(t.Context(), store.SessionStart{ProjectID: id, IssueID: id + "-issue", StartedAt: now.Add(-time.Hour), Model: id + "-model"})
		if err != nil {
			t.Fatal(err)
		}
		if err := deps.Store.FinishSession(t.Context(), session, store.SessionFinish{CompletedAt: now.Add(-time.Minute), TotalTokens: 10, FinalState: "completed", Model: id + "-model"}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := telemetry.Snapshot{GeneratedAt: now, Projects: []telemetry.ProjectSnapshot{{Project: telemetry.Project{ID: "detent", DisplayName: "A"}}, {Project: telemetry.Project{ID: "other", DisplayName: "B"}}}, WorkAttempts: []telemetry.WorkAttempt{{AttemptID: 1, ProjectID: "detent", Identifier: "detent-issue", Status: "running"}, {AttemptID: 2, ProjectID: "other", Identifier: "other-issue", Status: "running"}}, Events: []telemetry.ActivityEvent{{At: now, Message: "unattributed-secret"}}}
	if err := deps.Hub.Publish(snapshot); err != nil {
		t.Fatal(err)
	}
	server := fleetTestServer(t, deps)
	for _, ids := range [][]string{{"detent"}, {"other"}, {"detent", "other"}} {
		key, err := apikey.NewService(deps.Store).Create(t.Context(), apikey.CreateRequest{Name: "analytics", Scopes: []string{"read"}, ProjectIDs: ids})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
			t.Run(strings.Join(ids, ",")+"/"+name, func(t *testing.T) {
				response, raw := fleetCall(t, server, key.Token, name, map[string]any{})
				if response.Code != http.StatusOK || len(raw) == 0 {
					t.Fatalf("read %s", response.Body.String())
				}
				if strings.Contains(string(raw), "unattributed-secret") {
					t.Fatalf("unattributed events: %s", raw)
				}
				if len(ids) == 1 {
					other := "other"
					if ids[0] == "other" {
						other = "detent"
					}
					if strings.Contains(string(raw), other+"-model") || strings.Contains(string(raw), other+"-issue") || strings.Contains(string(raw), `"project_id":"`+other+`"`) {
						t.Fatalf("foreign scope: %s", raw)
					}
				}
				if name == operatortool.Reports {
					for _, field := range []string{`"digest"`, `"efficiency"`, `"cost_per_outcome"`, `"usage"`, `"generated_at"`, `"observed_at"`} {
						if !strings.Contains(string(raw), field) {
							t.Fatalf("missing %s: %s", field, raw)
						}
					}
				}
				for _, id := range ids {
					if name == operatortool.AnalyticsDashboard && !strings.Contains(string(raw), id+"-issue") || name == operatortool.Reports && !strings.Contains(string(raw), id+"-model") {
						t.Fatalf("missing authorized data for %s: %s", id, raw)
					}
				}
				if name == operatortool.Reports && len(ids) == 2 {
					for offset := range 2 {
						_, raw := fleetCall(t, server, key.Token, name, map[string]any{"limit": 1, "offset": offset})
						var page struct {
							Projects []struct {
								ProjectID string `json:"project_id"`
							} `json:"projects"`
							HasMore bool `json:"has_more"`
						}
						if err := json.Unmarshal(raw, &page); err != nil || len(page.Projects) != 1 || page.Projects[0].ProjectID != ids[offset] || page.HasMore != (offset == 0) {
							t.Fatalf("report project page %d: %s error=%v", offset, raw, err)
						}
					}
				}
				bridge := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, `{}`, map[string]string{"Authorization": "Bearer " + key.Token})
				if bridge.Code != http.StatusOK || !strings.Contains(bridge.Body.String(), `"observed_at"`) {
					t.Fatalf("stdio bridge=%d %s", bridge.Code, bridge.Body.String())
				}
				for _, args := range []string{`{"project_id":"foreign"}`} {
					denied := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, args, map[string]string{"Authorization": "Bearer " + key.Token})
					if denied.Code < 400 {
						t.Fatalf("accepted %s: %s", args, denied.Body.String())
					}
				}
			})
		}
		if err := deps.Store.RevokeAPIKey(t.Context(), key.Key.ID, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
			response, _ := fleetCall(t, server, key.Token, name, map[string]any{})
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("revoked call=%d %s", response.Code, response.Body.String())
			}
		}
	}
	for _, test := range []struct{ name, args string }{
		{operatortool.AnalyticsDashboard, `{"limit":201}`}, {operatortool.AnalyticsDashboard, `{"offset":-1}`}, {operatortool.AnalyticsDashboard, `{"confirmation":true}`},
		{operatortool.TimeSeries, `{"window":"-1m"}`}, {operatortool.TimeSeries, `{"window":"25h"}`}, {operatortool.TimeSeries, `{"window":"24h","bucket":"1ns"}`},
		{operatortool.TimeSeries, `{"window":"24h","bucket":"1h"}`}, {operatortool.Reports, `{"from":"2020-01-01"}`}, {operatortool.Reports, `{"from":"bad"}`}, {operatortool.Reports, `{"from":"2026-10-02","to":"2026-10-01"}`}, {operatortool.Reports, `{"bucket":"1ns"}`}, {operatortool.Reports, `{"tz":"missing/zone"}`},
	} {
		t.Run(test.args, func(t *testing.T) {
			response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+test.name, test.args, map[string]string{"Authorization": "Bearer detent_admin_token"})
			want := http.StatusBadRequest
			if test.args == `{"window":"24h","bucket":"1h"}` {
				want = http.StatusOK
			}
			if response.Code != want {
				t.Fatalf("%d want %d: %s", response.Code, want, response.Body.String())
			}
		})
	}
	oversized := snapshot
	oversized.GeneratedAt = now.Add(time.Second)
	oversized.WorkAttempts = []telemetry.WorkAttempt{{ProjectID: "detent", StatusMessage: strings.Repeat("x", operatortool.MaxResultBytes+1)}}
	if err := deps.Hub.Publish(oversized); err != nil {
		t.Fatal(err)
	}
	response, raw := fleetCall(t, server, "detent_admin_token", operatortool.AnalyticsDashboard, map[string]any{})
	if len(raw) != 0 || !strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("oversize %s", response.Body.String())
	}
}

func TestMCPAnalyticsNativeProject(t *testing.T) {
	server, fixture := newNativeWebServer(t)
	for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
		response, raw := fleetCall(t, server, fixture.keys["readnative"], name, map[string]any{"project_id": "native"})
		if response.Code != http.StatusOK || len(raw) == 0 || strings.Contains(string(raw), `"project_id":"other"`) {
			t.Fatalf("native read %s", response.Body.String())
		}
	}
}
