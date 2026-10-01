package web_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/web"
)

// Verifies real browser approval and credential delivery through the dashboard
// command: scoped administrators cannot create keys, credentials never enter
// previews/audits/receipts, and revocation invalidates a previously delivered result.
func TestMCPCredentialAdministration(t *testing.T) {
	deps := testDeps(t)
	deps.Store = openWebTestStore(t)
	var logs bytes.Buffer

	server, err := web.NewServer(web.Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), GlobalConfig: globalconfig.Config{APIToken: "detent_admin_token", DashboardAccess: globalconfig.DashboardAccess{Mode: globalconfig.DashboardAccessModePrivateToken, Token: "human-only-fixture", AllowWrite: true}}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	connect := func(token string) string {
		response := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-connections", `{}`, map[string]string{"Authorization": "Bearer " + token})
		var reply struct {
			ID string `json:"connection_id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &reply); err != nil || reply.ID == "" {
			t.Fatalf("connection=%d %s %v", response.Code, response.Body, err)
		}
		return reply.ID
	}
	id := connect("detent_admin_token")
	call := func(connection, token, name, args string) string {
		r := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/operator-tools/"+name, args, map[string]string{"Authorization": "Bearer " + token, "X-Detent-Connection-ID": connection})
		return r.Body.String()
	}
	// The stdio bridge's typed application API and MCP use the same dispatcher.
	args := `{"request_id":"create-once","name":"MCP created","scopes":["read"],"project_ids":["detent"]}`
	pending := call(id, "detent_admin_token", "credential_create", args)
	var reply struct {
		Action struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"action"`
	}
	if err := json.Unmarshal([]byte(pending), &reply); err != nil || reply.Action.Status != "pending" {
		t.Fatalf("preview=%s %v", pending, err)
	}
	entry := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/?token=human-only-fixture"})
	cookies := entry.Result().Cookies()
	approve := func(connection, actionID string) {
		view := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + connection, cookies: cookies})
		if view.Code == http.StatusSeeOther {
			cookies = append(cookies, view.Result().Cookies()...)
			view = performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + connection, cookies: cookies})
		}
		cookies = append(cookies, view.Result().Cookies()...)
		block := ""
		for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(view.Body.String(), -1) {
			if strings.Contains(form, `name="action_id" value="`+actionID+`"`) {
				block = form
			}
		}
		match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(block)
		if len(match) != 2 {
			t.Fatalf("approval form=%d %s", view.Code, view.Body)
		}
		decision := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{method: http.MethodPost, path: "/chat/approval", cookies: cookies, form: url.Values{"connection_id": {connection}, "action_id": {actionID}, "form_token": {match[1]}, "decision": {"confirm"}}})
		if decision.Code != http.StatusSeeOther {
			t.Fatalf("decision=%d %s", decision.Code, decision.Body)
		}
	}
	approve(id, reply.Action.ID)

	delivered := call(id, "detent_admin_token", "action_result", `{"action_id":"`+reply.Action.ID+`"}`)
	var output struct {
		Output struct {
			ResourceID string `json:"resource_id"`
			Data       struct {
				Token string `json:"token"`
			} `json:"data"`
		} `json:"output"`
	}
	if err := json.Unmarshal([]byte(delivered), &output); err != nil || output.Output.Data.Token == "" {
		t.Fatalf("delivered=%s %v", delivered, err)
	}
	secret := output.Output.Data.Token
	view := performDashboardHTMXRequest(t, server.Handler(), dashboardHTMXRequest{path: "/chat/approval?connection_id=" + id, cookies: cookies})
	if strings.Contains(view.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("credential leaked into browser or audit")
	}
	if _, err := deps.Store.APIKeyByHash(t.Context(), apikey.HashToken(secret)); err != nil {
		t.Fatal(err)
	}

	if replay := call(id, "detent_admin_token", "credential_create", args); !strings.Contains(replay, secret) {
		t.Fatalf("bound retry lost result: %s", replay)
	}
	// Reconnect retries are durable, but omit the credential.
	reconnected := connect("detent_admin_token")
	reconnect := call(reconnected, "detent_admin_token", "credential_create", args)
	var retry struct {
		Action struct {
			ID string `json:"id"`
		} `json:"action"`
	}
	if err := json.Unmarshal([]byte(reconnect), &retry); err != nil {
		t.Fatal(err)
	}
	approve(reconnected, retry.Action.ID)
	receipt := call(reconnected, "detent_admin_token", "action_result", `{"action_id":"`+retry.Action.ID+`"}`)
	if strings.Contains(receipt, secret) || !strings.Contains(receipt, output.Output.ResourceID) {
		t.Fatalf("durable receipt=%s", receipt)
	}
	keys, err := deps.Store.ListAPIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatalf("reconnect duplicated keys=%d %v", len(keys), err)
	}
	rotated := performJSON(t, server.Handler(), http.MethodPost, "/api/v1/keys/"+output.Output.ResourceID+"/rotate", `{"grace":"1h"}`, map[string]string{"Authorization": "Bearer detent_admin_token"})
	if rotated.Code != http.StatusCreated {
		t.Fatalf("rotate=%d %s", rotated.Code, rotated.Body)
	}
	if response := performJSON(t, server.Handler(), http.MethodDelete, "/api/v1/keys/"+output.Output.ResourceID, `{}`, map[string]string{"Authorization": "Bearer detent_admin_token"}); response.Code != http.StatusOK && response.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d %s", response.Code, response.Body)
	}
	if replay := call(id, "detent_admin_token", "action_result", `{"action_id":"`+reply.Action.ID+`"}`); strings.Contains(replay, secret) {
		t.Fatal("revoked credential result replayed")
	}
	for _, scopes := range [][]string{{"read"}, {"write"}, {"admin"}} {
		token, _ := createRemoteMCPKey(t, server, "Scoped "+scopes[0], scopes, []string{"detent"})
		if result := call(connect(token), token, "credential_create", args); strings.Contains(result, `"status":"pending"`) {
			t.Fatalf("scoped %s created key: %s", scopes, result)
		}
	}
}
