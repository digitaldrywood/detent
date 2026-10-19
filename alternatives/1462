package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func coordinatorReadFixture(t *testing.T) (hostedSecurityFixture, hostedSecurityUser, conversationRecord) {
	t.Helper()
	now := time.Now().UTC()
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.now = func() time.Time { return now }
		cfg.Conversation = &ConversationConfig{Enabled: true, Backend: newFakeCoordinatorBackend(), Workspace: t.TempDir()}
		cfg.Version = "v0.117.99"
	})
	owner := f.user(t, "screen-owner", "owner", "owner@example.test", "write", "")
	seedCoordinatorRepository(t, f.service, f.project)
	if _, err := f.service.database.approvePolicy(t.Context(), "org_security/"+string(f.project), "test", policy.Change{Policy: hubTestPolicy()}); err != nil {
		t.Fatal(err)
	}
	response := f.request(t, owner, http.MethodPost, f.base+"/conversations", map[string]any{"key": "screen-chat"})
	requireNativeStatus(t, response, http.StatusCreated)
	var created struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	decodeHubResponse(t, response, &created)
	operatorSQL(t, f, "UPDATE conversations SET visibility='shared' WHERE id=?", created.Conversation.ID)
	c := f.service.conversations.coordinator.(*conversationTurnCoordinator)
	record, err := c.readConversation(t.Context(), f.service.database.db, created.Conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, owner, record
}

func coordinatorViewerTools(t *testing.T, f hostedSecurityFixture, record conversationRecord, user hostedSecurityUser) (*coordinatorToolset, context.Context) {
	t.Helper()
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/coordinator-read-context", func(c echo.Context) error {
		actorCtx, err := f.service.bindCoordinatorConnection(c.Request().Context(), record.ID+":"+user.identity.Subject)
		if err != nil {
			return err
		}
		contexts <- actorCtx
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	response := f.request(t, user, http.MethodGet, "/api/v2/organizations/org_security/coordinator-read-context", nil)
	requireNativeStatus(t, response, http.StatusOK)
	actorCtx := <-contexts
	if actorCtx == nil {
		t.Fatal("missing viewer context")
	}
	c := f.service.conversations.coordinator.(*conversationTurnCoordinator)
	state := &coordinatorTurnState{coordinator: c, conversationID: record.ID, users: []conversationMessageRecord{{Role: conversation.RoleUser, Text: "Read project screens", Data: coordinatorConnectionData(actorCtx)}}}
	return newCoordinatorToolset(c, state), actorCtx
}

func TestCoordinatorScreenReads(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, role := range []struct {
		name, role string
		runner     bool
	}{
		{"owner", "owner", false}, {"admin", "admin", false}, {"member", "member", false}, {"runner member", "member", true}, {"viewer", "viewer", false},
	} {
		t.Run(role.name, func(t *testing.T) {
			f, _, record := coordinatorReadFixture(t)
			user := f.user(t, "screen-viewer", role.role, "viewer@example.test", "read", "")
			f.grant(t, user, false, role.runner)
			seedHostedContextRunner(t, f, "attached", "v0.117.99", f.service.config.now(), false)
			operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id='tok_attached'", formatHubTime(f.service.config.now().Add(time.Hour)))
			operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES('tok_attached','org_security',?)", f.project)
			issueID := f.seedIssue(t, 1)
			tools, ctx := coordinatorViewerTools(t, f, record, user)
			catalog := tools.tools()
			cases := []struct {
				name, args string
				allowed    bool
				permission string
			}{
				{"get_slack_integration", `{}`, role.role == "owner" || role.role == "admin", "Slack integration settings"},
				{"get_project_budget", `{}`, true, ""},
				{operatortool.GetOrganizationModelSelection, `{}`, true, ""},
				{operatortool.BoardActivity, fmt.Sprintf(`{"reference":%q}`, issueID), true, ""},
				{"get_project_fleet", `{}`, true, ""},
				{"get_project_members", `{}`, true, ""},
				{"get_project_runners", `{}`, role.role == "owner" || role.role == "admin" || role.runner, "runner details"},
				{operatortool.GetRunnerCapacity, `{"runner_id":"attached"}`, role.role == "owner" || role.role == "admin" || role.runner, "runner details"},
				{operatortool.GetRunnerUpdate, `{"runner_id":"attached"}`, role.role == "owner" || role.role == "admin" || role.runner, "runner details"},
				{"get_billing_status", `{}`, role.role == "owner", "billing"},
				{"get_billing_usage", `{}`, role.role == "owner", "billing"},
				{"get_organization_plan", `{}`, role.role == "owner" || role.role == "admin", "plan details"},
				{operatortool.MembershipList, `{}`, role.role == "owner" || role.role == "admin", "member management"},
				{"get_native_project", `{}`, true, ""}, {"get_onboarding", `{}`, true, ""}, {"project_setup", `{}`, true, ""},
				{"get_project_policy", `{}`, true, ""}, {operatortool.GetProjectModelSelection, `{}`, true, ""},
				{"project_secret_metadata", `{}`, true, ""}, {"repository_freshness", `{}`, true, ""}, {"monthly_usage_costs", `{}`, true, ""},
				{operatortool.WorkList, `{}`, true, ""}, {operatortool.HealthFindings, `{}`, true, ""}, {operatortool.BoardState, `{}`, true, ""},
				{operatortool.TimeSeries, `{"window":"1h","bucket":"5m"}`, true, ""},
				{operatortool.TimeSeries, fmt.Sprintf(`{"from":%q,"to":%q,"bucket":"5m"}`, f.service.config.now().Add(-49*time.Hour).Format(time.RFC3339Nano), f.service.config.now().Add(-48*time.Hour).Format(time.RFC3339Nano)), true, ""},
				{operatortool.AnalyticsDashboard, `{}`, true, ""}, {operatortool.Reports, `{}`, true, ""}, {"get_project_usage", `{"range":"24h"}`, true, ""},
				{"get_project_diagnostics", `{}`, true, ""}, {"get_project_reports", `{}`, true, ""},
			}
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					if !slices.ContainsFunc(catalog, func(tool runner.AgentTool) bool { return tool.Name == test.name }) {
						t.Fatal("read is absent from coordinator catalog")
					}
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: test.name, Arguments: json.RawMessage(test.args)})
					if err != nil {
						t.Fatal(err)
					}
					if !test.allowed {
						if result.Success || !strings.Contains(result.Content, "you don't have access to "+test.permission) {
							t.Fatalf("want named refusal, got %#v", result)
						}
						return
					}
					if !result.Success {
						t.Fatalf("allowed screen read failed: %s", result.Content)
					}
					if test.name == operatortool.TimeSeries && strings.Contains(test.args, `"from"`) {
						var report nativeAnalyticsReport
						if err := json.Unmarshal([]byte(result.Content), &report); err != nil {
							t.Fatal(err)
						}
						if !report.Window.From.Equal(f.service.config.now().Add(-49*time.Hour)) || !report.Window.To.Equal(f.service.config.now().Add(-48*time.Hour)) {
							t.Fatalf("historical interval was not preserved: %+v", report.Window)
						}
					}
					if strings.Contains(result.Content, "ciphertext") || strings.Contains(result.Content, "token_hash") {
						t.Fatalf("unsafe result: %s", result.Content)
					}
					if coordinatorProjectReadScoped(test.name) && test.name != operatortool.GetRunnerUpdate && test.name != operatortool.GetRunnerCapacity && test.name != "get_project_budget" && test.name != "get_slack_integration" && test.name != "get_project_runners" && test.name != "get_project_fleet" && test.name != "get_project_diagnostics" && test.name != "get_project_reports" {
						fields := map[string]json.RawMessage{}
						if err := json.Unmarshal([]byte(test.args), &fields); err != nil {
							t.Fatal(err)
						}
						fields["project_id"], err = json.Marshal(f.project)
						if err != nil {
							t.Fatal(err)
						}
						if test.name == "monthly_usage_costs" {
							fields["scope"] = json.RawMessage(`"project"`)
						}
						raw, err := json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
						direct, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: coordinatorScreenTarget(test.name), Arguments: raw})
						if err != nil {
							t.Fatal(err)
						}
						var got, want any
						if json.Unmarshal([]byte(result.Content), &got) != nil || json.Unmarshal(direct.Content, &want) != nil {
							t.Fatal("invalid result")
						}
						if test.name == "get_project_policy" {
							gates := want.(map[string]any)["data"].(map[string]any)["policy"].(map[string]any)["gates"].(map[string]any)
							for _, key := range []string{"human_review", "github_pull_request"} {
								if _, present := gates[key]; !present {
									gates[key] = false
								}
							}
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("coordinator diverged from application read: %s vs %s", result.Content, direct.Content)
						}
					}
				})
			}
		})
	}
}

func TestCoordinatorScreenReadBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name, args string
		revoke     bool
	}{
		{"foreign project", `{"project_id":"prj_other"}`, false}, {"organization scope", `{"scope":"organization"}`, false},
		{"foreign organization", `{"organization_id":"org_foreign"}`, false},
		{"unknown field", `{"secret":true}`, false}, {"revoked project grant", `{}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, owner, record := coordinatorReadFixture(t)
			tools, _ := coordinatorViewerTools(t, f, record, owner)
			if test.revoke {
				operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=?", owner.identity.Subject)
			}
			for _, tool := range coordinatorScreenTools() {
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool.Name, Arguments: json.RawMessage(test.args)})
				if err != nil || result.Success {
					t.Fatalf("%s crossed boundary: %#v, %v", tool.Name, result, err)
				}
			}
		})
	}
}

func TestCoordinatorWorkListRefusalNamesReason(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	f, owner, record := coordinatorReadFixture(t)
	tools, _ := coordinatorViewerTools(t, f, record, owner)
	for _, test := range []struct{ args, reason string }{
		{`{"state":"Triage","include":["summary","work"]}`, "include accepts summary or work, not both"},
		{`{"state":"Triage","offset":20}`, "offset is not supported here; page with the returned next_cursor"},
	} {
		result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.WorkList, Arguments: json.RawMessage(test.args)})
		if err != nil || result.Success || !strings.Contains(result.Content, test.reason) {
			t.Fatalf("%s: want refusal naming %q, got %#v %v", test.args, test.reason, result, err)
		}
	}
}

func TestCoordinatorReadAudience(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	f, owner, record := coordinatorReadFixture(t)
	tools, _ := coordinatorViewerTools(t, f, record, owner)
	result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: "get_native_project", Arguments: json.RawMessage(`{}`)})
	if err != nil || !result.Success {
		t.Fatalf("read: %#v %v", result, err)
	}
	for _, test := range []struct {
		name, role, grant string
		providerDowngrade bool
		status            int
	}{
		{"same access", "owner", "write", false, http.StatusOK}, {"lower role", "member", "write", false, http.StatusNotFound},
		{"different grants", "owner", "read", false, http.StatusNotFound},
		{"provider downgrade", "owner", "write", true, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			viewer := f.user(t, test.name, test.role, test.name+"@example.test", test.grant, "")
			if test.providerDowngrade {
				f.provider.mu.Lock()
				for id, member := range f.provider.members {
					if member.UserID == viewer.identity.Subject {
						member.Role.Slug = "member"
						f.provider.members[id] = member
					}
				}
				f.provider.mu.Unlock()
			}
			response := f.request(t, viewer, http.MethodGet, f.base+"/conversations/"+record.ID, nil)
			requireNativeStatus(t, response, test.status)
			response = f.request(t, viewer, http.MethodGet, f.base+"/conversations/"+record.ID+"/messages", nil)
			requireNativeStatus(t, response, test.status)
			response = f.request(t, viewer, http.MethodGet, f.base+"/conversations", nil)
			requireNativeStatus(t, response, http.StatusOK)
			listed := strings.Contains(response.Body.String(), record.ID)
			if listed != (test.status == http.StatusOK) {
				t.Fatalf("list leaked restricted chat: %s", response.Body.String())
			}
		})
	}
	operatorSQL(t, f, "UPDATE hosted_project_grants SET can_write=0 WHERE user_id=?", owner.identity.Subject)
	response := f.request(t, owner, http.MethodGet, f.base+"/conversations/"+record.ID, nil)
	requireNativeStatus(t, response, http.StatusNotFound)
	_, err = tools.execute(t.Context(), runner.AgentToolCall{Name: "get_native_project", Arguments: json.RawMessage(`{}`)})
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("changed owner access read persisted answers: %v", err)
	}
}

func TestCoordinatorProjectRunners(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	for _, test := range []struct {
		name         string
		count        int
		disabled     bool
		organization bool
	}{
		{"three healthy self-hosted runners", 3, false, false}, {"empty project", 0, false, false}, {"attached disabled runner", 1, true, false}, {"organization-scoped runner", 1, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, owner, record := coordinatorReadFixture(t)
			operatorSQL(t, f, `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT 'prj_other',organization_id,'foreign-project-sentinel',profile,states_json,created_at FROM projects WHERE id=?`, f.project)
			ids := []string{"self-hosted-one", "self-hosted-two", "self-hosted-three"}
			for _, id := range append(slices.Clone(ids[:test.count]), "unattached-sentinel") {
				seedHostedContextRunner(t, f, id, "v0.117.99", f.service.config.now(), false)
				operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(f.service.config.now().Add(time.Hour)), "tok_"+id)
				project := string(f.project)
				if id == "unattached-sentinel" {
					project = "prj_other"
				}

				operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,'org_security',?)", "tok_"+id, project)
			}
			if test.count > 0 {
				operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,'org_security','prj_other')", "tok_"+ids[0])
				operatorSQL(t, f, "UPDATE projects SET scheduling_rank=2 WHERE id='prj_other'")
			}
			if test.organization {
				operatorSQL(t, f, "UPDATE runner_identities SET scope='organization' WHERE id=?", ids[0])
				operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id=?", "tok_"+ids[0])
			}
			if test.count > 0 {
				problems, err := json.Marshal([]runnerauth.Problem{
					{Code: "keep_awake_failed", Message: "shared-host-problem"},
					{ProjectID: string(f.project), Code: "backend_missing", Message: "current-project-problem"},
					{ProjectID: "prj_other", Code: "settings_invalid", Message: "foreign-problem-sentinel", ErrorOutput: "foreign-error-sentinel", FixCommand: "foreign-command-sentinel"},
				})
				if err != nil {
					t.Fatal(err)
				}
				operatorSQL(t, f, "UPDATE runner_identities SET problems_json=? WHERE id=?", string(problems), ids[0])
			}
			if test.disabled {
				operatorSQL(t, f, "UPDATE runner_identities SET state='disabled' WHERE id=?", ids[0])
			}
			foreignMember := f.user(t, "foreign-member", "member", "foreign@example.test", "", "")
			operatorSQL(t, f, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id) VALUES(?,'org_security','prj_other')", foreignMember.identity.Subject)
			tools, _ := coordinatorViewerTools(t, f, record, owner)
			for _, read := range []string{"get_project_runners", "get_project_fleet"} {
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: read, Arguments: json.RawMessage(`{}`)})
				if err != nil || !result.Success {
					t.Fatalf("runners: %#v %v", result, err)
				}
				var view struct {
					Runners []struct {
						ID                    string                   `json:"id"`
						Connected             bool                     `json:"connected"`
						Heartbeat             time.Time                `json:"last_heartbeat_at"`
						Capacity              int                      `json:"reported_capacity"`
						Used                  int                      `json:"host_used"`
						Sprite                any                      `json:"sprite"`
						CapacityConfiguration *runnerauth.CapacityView `json:"capacity_configuration"`
						Routing               struct {
							Projects []string       `json:"project_ids"`
							Ranks    map[string]int `json:"project_ranks"`
						} `json:"routing"`
					} `json:"runners"`
					Options []string `json:"runner_options"`
				}
				if err := json.Unmarshal([]byte(result.Content), &view); err != nil {
					t.Fatal(err)
				}
				if len(view.Runners) != test.count {
					t.Fatalf("attached runners: %s", result.Content)
				}
				if !slices.Contains(view.Options, "self-hosted") || !slices.Contains(view.Options, "Sprites") {
					t.Fatalf("missing runner options: %s", result.Content)
				}
				for _, r := range view.Runners {
					if r.CapacityConfiguration == nil {
						t.Fatalf("missing capacity configuration: %s", result.Content)
					}
					if _, ok := r.CapacityConfiguration.Projects[f.project]; !ok {
						t.Fatalf("missing current project capacity: %s", result.Content)
					}
					if r.Connected == test.disabled || !r.Heartbeat.Equal(f.service.config.now()) || r.Capacity != 1 || r.Used != 0 || r.Sprite != nil {
						t.Fatalf("incorrect runner health/capacity: %s", result.Content)
					}
					if test.organization && len(r.Routing.Projects) != 0 || !test.organization && (len(r.Routing.Projects) != 1 || r.Routing.Projects[0] != string(f.project)) {
						t.Fatalf("foreign routing: %s", result.Content)
					}
				}
				for _, sentinel := range []string{"prj_other", "unattached-sentinel", "foreign-org", "tok_", "foreign-problem-sentinel", "foreign-error-sentinel", "foreign-command-sentinel"} {
					if strings.Contains(result.Content, sentinel) {
						t.Fatalf("foreign or credential data: %s", result.Content)
					}
				}
				if test.count > 0 && (!strings.Contains(result.Content, "shared-host-problem") || !strings.Contains(result.Content, "current-project-problem")) {
					t.Fatalf("missing visible runner problems: %s", result.Content)
				}
			}
			for _, name := range []string{"get_onboarding", "get_project_members", operatortool.MembershipList} {
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: name, Arguments: json.RawMessage(`{}`)})
				if err != nil || !result.Success || strings.Contains(result.Content, "prj_other") {
					t.Fatalf("%s exposed an ungranted project: %#v %v", name, result, err)
				}
			}
			for _, tool := range []string{operatortool.GetRunnerUpdate, operatortool.GetRunnerCapacity} {
				if test.organization {
					raw, err := json.Marshal(map[string]string{"runner_id": ids[0]})
					if err != nil {
						t.Fatal(err)
					}
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: raw})
					if err != nil || !result.Success {
						t.Fatalf("organization runner read: %#v %v", result, err)
					}
				}
				for _, id := range []string{"unattached-sentinel", "foreign-org-runner-sentinel"} {
					raw, err := json.Marshal(map[string]string{"runner_id": id})
					if err != nil {
						t.Fatal(err)
					}
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: raw})
					if err != nil || result.Success {
						t.Fatalf("foreign runner read: %#v %v", result, err)
					}
				}
			}
		})
	}
}
