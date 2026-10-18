package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
)

func coordinatorSettingsArguments(t *testing.T, f hostedSecurityFixture, tool, runnerID string) json.RawMessage {
	t.Helper()
	var value any = map[string]any{"input": map[string]any{}}
	switch tool {
	case operatortool.UpdateRunnerCapacity, operatortool.UpdateRunnerRouting:
		seedHostedContextRunner(t, f, runnerID, "test", f.service.config.now(), false)
		operatorSQL(t, f, "UPDATE api_tokens SET expires_at=? WHERE id=?", formatHubTime(f.service.config.now().Add(time.Hour)), "tok_"+runnerID)
		operatorSQL(t, f, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,'org_security',?)", "tok_"+runnerID, f.project)
		config := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 1, ClientLimit: 1, RuntimeLimit: 1, Manageable: true, ObservedAt: f.service.config.now()}
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		operatorSQL(t, f, "UPDATE runner_identities SET capacity_configuration_json=? WHERE id=?", string(raw), runnerID)
		r, err := readRunnerWithClock(t.Context(), f.service.database.db, "org_security", runnerID, f.service.config.now)
		if err != nil {
			t.Fatal(err)
		}
		if tool == operatortool.UpdateRunnerCapacity {
			value = map[string]any{"runner_id": r.RunnerID, "change": runnerCapacityChange{ExpectedRevision: r.Revision, CapacityRequest: runnerauth.CapacityRequest{ExpectedConfigRevision: config.Revision, Capacity: 6}}}
		} else {
			r.ProjectRanks[f.project] = 7
			value = map[string]any{"runner_id": r.RunnerID, "change": runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{Routing: r.Routing, ExpectedRevision: r.Revision}}}
		}
	case operatortool.UpdateProjectModelSelection:
		value = map[string]any{"input": map[string]any{"expected_revision": "1", "selection": map[string]any{"normal_model": "chat-model"}}}
	case "bind_native_repository":
		value = map[string]any{"input": map[string]any{"expected_revision": "1", "repository": "example/repo", "source": "runner_checkout"}}
	case "approve_project_policy":
		value = map[string]any{"input": map[string]any{"expected_policy_id": hubTestPolicy().ID, "policy": hubTestPolicy()}}
	case "revoke_project_policy":
		value = map[string]any{"input": map[string]any{"expected_policy_id": hubTestPolicy().ID}}
	case "save_onboarding":
		value = map[string]any{"input": map[string]any{"progress": map[string]any{"revision": "0", "repository": "existing"}}}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func coordinatorSettingsProposal(t *testing.T, f hostedSecurityFixture, conversationID string) chat.Action {
	t.Helper()
	messages, err := f.service.conversations.store.listMessages(t.Context(), f.service.database.db, conversationID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		var data struct {
			OperatorAction struct {
				Action chat.Action `json:"action"`
			} `json:"operator_action"`
		}
		if json.Unmarshal(message.Data, &data) == nil && data.OperatorAction.Action.ID != "" {
			return data.OperatorAction.Action
		}
	}
	t.Fatal("no confirmable settings action")
	return chat.Action{}
}

func TestCoordinatorSettingsActions(t *testing.T) {
	for _, tool := range coordinatorSettingsNames() {
		roles := []string{"owner", "admin", "member", "runner member", "viewer"}
		if tool == operatortool.UpdateRunnerCapacity {
			roles = append(roles, "organization owner")
		}
		for _, role := range roles {
			t.Run(tool+"/"+role, func(t *testing.T) {
				f, _, record := coordinatorReadFixture(t)
				name := role
				if role == "organization owner" {
					name = "owner"
				}
				if role == "runner member" {
					name = "member"
				}
				u := f.user(t, "settings-viewer", name, "settings@example.test", "write", "")
				f.grant(t, u, name != "viewer", role == "runner member")
				args := coordinatorSettingsArguments(t, f, tool, "settings-runner")
				if role == "organization owner" {
					operatorSQL(t, f, "UPDATE runner_identities SET scope='organization' WHERE id='settings-runner'")
					operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id='tok_settings-runner'")
				}
				tools, _ := coordinatorViewerTools(t, f, record, u)
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: args})
				want := name == "owner" || name == "admin" || role == "runner member" && hubFleetTool(tool) || name == "member" && tool == "save_onboarding"
				if err != nil || result.Success != want {
					t.Fatalf("proposal = %+v, %v; allow=%t", result, err, want)
				}
				if !want {
					if !strings.Contains(result.Content, "access") || !strings.Contains(result.Content, "Settings") {
						t.Fatalf("missing named permission/screen refusal: %s", result.Content)
					}
					return
				}
				action := coordinatorSettingsProposal(t, f, record.ID)
				if hubFleetTool(tool) {
					r, err := readRunnerWithClock(t.Context(), f.service.database.db, record.OrganizationID, "settings-runner", f.service.config.now)
					if err != nil || r.CapacityLimit != 1 || r.Revision != 1 || r.CapacityRequest != nil {
						t.Fatalf("preview changed runner configuration: %+v, %v", r, err)
					}
				}
				if string(action.Kind) != tool || action.ProjectID != string(f.project) || action.RequestID == "" || !strings.Contains(result.Content, "proposed") {
					t.Fatalf("incorrect proposal: %+v, %s", action, result.Content)
				}
				{
					response := f.request(t, u, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
					if tool == "revoke_project_policy" {
						requireNativeStatus(t, response, http.StatusGone)
						if !strings.Contains(response.Body.String(), `"code":"operation_retired"`) {
							t.Fatalf("revocation did not report retirement: %s", response.Body.String())
						}
						return
					}
					requireNativeStatus(t, response, http.StatusOK)
					if tool == operatortool.UpdateRunnerCapacity {
						if !strings.Contains(response.Body.String(), `\"desired\":6`) || !strings.Contains(response.Body.String(), "application evidence") {
							t.Fatalf("capacity confirmation omitted read-back: %s", response.Body.String())
						}
						r, err := readRunnerWithClock(t.Context(), f.service.database.db, record.OrganizationID, "settings-runner", f.service.config.now)
						if err != nil || r.CapacityLimit != 6 || r.CapacityRequest == nil || r.CapacityRequest.Capacity != 6 {
							t.Fatalf("capacity intent not applied: %+v, %v", r, err)
						}
					}
				}
			})
		}
	}
}

func TestCoordinatorSettingsBoundaries(t *testing.T) {
	for _, tool := range coordinatorSettingsNames() {
		for _, scenario := range []string{"foreign project", "unknown field", "revoked grant", "unattached runner", "stale runner"} {
			if !hubFleetTool(tool) && (scenario == "unattached runner" || scenario == "stale runner") {
				continue
			}
			t.Run(tool+"/"+scenario, func(t *testing.T) {
				f, owner, record := coordinatorReadFixture(t)
				args := coordinatorSettingsArguments(t, f, tool, "settings-runner")
				tools, _ := coordinatorViewerTools(t, f, record, owner)
				if scenario == "revoked grant" || scenario == "stale runner" {
					result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: args})
					if err != nil || !result.Success {
						t.Fatalf("preview: %+v, %v", result, err)
					}
					action := coordinatorSettingsProposal(t, f, record.ID)
					if scenario == "revoked grant" {
						operatorSQL(t, f, "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", owner.identity.Subject, f.project)
					} else {
						operatorSQL(t, f, "UPDATE runner_identities SET revision=revision+1 WHERE id='settings-runner'")
					}
					response := f.request(t, owner, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
					if response.Code == http.StatusOK {
						t.Fatalf("stale/revoked action succeeded: %s", response.Body.String())
					}
					return
				}
				var fields map[string]json.RawMessage
				if json.Unmarshal(args, &fields) != nil {
					t.Fatal("invalid fixture")
				}
				if scenario == "foreign project" {
					fields["project_id"] = json.RawMessage(`"prj_other"`)
				}
				if scenario == "unknown field" {
					fields["secret_value"] = json.RawMessage(`"never-store-this"`)
				}
				if scenario == "unattached runner" {
					operatorSQL(t, f, "DELETE FROM token_grants WHERE token_id='tok_settings-runner'")
				}
				raw, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: tool, Arguments: raw})
				if err != nil || result.Success {
					t.Fatalf("boundary accepted: %+v, %v", result, err)
				}
			})
		}
	}
}

func TestConfigurationMapOwners(t *testing.T) {
	f, _, _ := coordinatorReadFixture(t)
	_, body, err := skills.ReadBuiltin("configuration-map")
	if err != nil {
		t.Fatal(err)
	}
	sections, err := os.ReadFile("../../web/conversation/src/app/settings/sections.tsx")
	if err != nil {
		t.Fatal(err)
	}
	routes := f.service.echo.Routes()
	owners := []string{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| ") || !strings.Contains(line, "/settings/") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) != 8 {
			t.Fatalf("invalid map row: %s", line)
		}
		section := strings.TrimPrefix(strings.TrimSpace(cells[2]), "/settings/")
		if !strings.Contains(string(sections), fmt.Sprintf("\"%s\"", section)) {
			t.Fatalf("unknown Cloud section %s", section)
		}
		for _, cell := range []string{cells[3], cells[4]} {
			name := strings.TrimSpace(cell)
			owners = append(owners, name)
			if _, ok := operatortool.Lookup(name); !ok && !slices.Contains([]string{"get_project_runners", "get_project_integration", "get_sprite_pool", "set_sprite_pool"}, name) {
				t.Fatalf("unknown configuration owner %s", name)
			}
		}
		endpoint := strings.Fields(cells[6])
		if len(endpoint) != 2 || !slices.ContainsFunc(routes, func(route *echo.Route) bool { return route.Method == endpoint[0] && route.Path == endpoint[1] }) {
			t.Fatalf("missing configuration route: %s", cells[6])
		}
	}
	for _, name := range coordinatorSettingsNames() {
		if !slices.Contains(owners, name) {
			t.Fatalf("settings tool %s is missing from the configuration map", name)
		}
	}
}

func TestCoordinatorSettingsCapacityBatch(t *testing.T) {
	f, owner, record := coordinatorReadFixture(t)
	tools, _ := coordinatorViewerTools(t, f, record, owner)
	ids := []string{"settings-one", "settings-two", "settings-three"}
	for _, id := range ids {
		args := coordinatorSettingsArguments(t, f, operatortool.UpdateRunnerCapacity, id)
		result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: operatortool.UpdateRunnerCapacity, Arguments: args})
		if err != nil || !result.Success {
			t.Fatalf("capacity preview for %s: %+v, %v", id, result, err)
		}
	}
	messages, err := f.service.conversations.store.listMessages(t.Context(), f.service.database.db, record.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	requests := map[string]bool{}
	confirmed := []string{}
	for _, message := range messages {
		var data struct {
			Proposal struct {
				Action chat.Action `json:"action"`
			} `json:"operator_action"`
		}
		if json.Unmarshal(message.Data, &data) != nil || data.Proposal.Action.ID == "" {
			continue
		}
		action := data.Proposal.Action
		if requests[action.RequestID] || !slices.Contains(ids, action.IssueID) {
			t.Fatalf("batch has duplicate or unrelated action: %+v", action)
		}
		requests[action.RequestID] = true
		response := f.request(t, owner, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action)
		requireNativeStatus(t, response, http.StatusOK)
		requireNativeStatus(t, f.request(t, owner, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
		r, err := readRunnerWithClock(t.Context(), f.service.database.db, record.OrganizationID, action.IssueID, f.service.config.now)
		if err != nil || r.CapacityLimit != 6 || r.Revision != 2 {
			t.Fatalf("runner applied more than once or unchanged: %+v, %v", r, err)
		}
		confirmed = append(confirmed, action.IssueID)
	}
	slices.Sort(confirmed)
	slices.Sort(ids)
	if !slices.Equal(confirmed, ids) {
		t.Fatalf("confirmed runners = %v, want %v", confirmed, ids)
	}
}
