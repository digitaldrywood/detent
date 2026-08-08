package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestOperatorToolAPIUsesSharedReadOnlyExecutor(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 8, 2, 30, 0, 0, time.UTC)
	expiresAt := observedAt.Add(15 * time.Minute)
	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{
		GeneratedAt:    observedAt,
		LastKnown:      true,
		LastKnownUntil: expiresAt,
		BoardIssues: []telemetry.Issue{
			{ID: "issue-1", Identifier: "digitaldrywood/detent#1", ProjectID: "detent", State: "Todo"},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "read-token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	recorder := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/board_state", `{"project_id":"detent","limit":1}`, map[string]string{
		"Authorization": "Bearer read-token",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var result operatortool.BoardStateResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Freshness != explain.SourceLastKnown || result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) || len(result.Items) != 1 {
		t.Fatalf("result = %#v", result)
	}
}

func TestOperatorToolAPIRejectsMutationAndMalformedCalls(t *testing.T) {
	t.Parallel()

	deps := testDeps(t)
	if err := deps.Hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "read-token"}}, deps)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	tests := []struct {
		name       string
		tool       string
		arguments  string
		wantStatus int
		wantCode   string
	}{
		{name: "proposal mutation", tool: "propose_move_item", arguments: `{}`, wantStatus: http.StatusNotFound, wantCode: "unknown_operator_tool"},
		{name: "unknown tool", tool: "missing", arguments: `{}`, wantStatus: http.StatusNotFound, wantCode: "unknown_operator_tool"},
		{name: "malformed JSON", tool: "board_state", arguments: `{"limit":`, wantStatus: http.StatusBadRequest, wantCode: "invalid_arguments"},
		{name: "unknown argument", tool: "fleet_health", arguments: `{"mutation":true}`, wantStatus: http.StatusBadRequest, wantCode: "invalid_arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+test.tool, test.arguments, map[string]string{
				"Authorization": "Bearer read-token",
			})
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			var problem struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if problem.Error.Code != test.wantCode {
				t.Fatalf("problem code = %q, want %q", problem.Error.Code, test.wantCode)
			}
		})
	}
}

func TestOperatorToolAPIUnavailableIsNotEmptyResult(t *testing.T) {
	t.Parallel()

	server, err := web.NewServer(web.Config{GlobalConfig: globalconfig.Config{APIToken: "read-token"}}, testDeps(t))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	recorder := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/recent_activity", `{}`, map[string]string{
		"Authorization": "Bearer read-token",
	})
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"snapshot_unavailable"`) {
		t.Fatalf("response = %d %s, want explicit snapshot_unavailable", recorder.Code, recorder.Body.String())
	}
}
