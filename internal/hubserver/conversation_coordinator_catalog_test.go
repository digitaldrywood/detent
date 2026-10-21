package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestCoordinatorCatalogMatchesOperatorCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	counts := map[string]int{}
	for _, role := range []struct{ name, role, grant string }{
		{"owner", "owner", "write"}, {"admin", "admin", "write"}, {"member", "member", "write"}, {"viewer", "viewer", "read"},
	} {
		t.Run(role.name, func(t *testing.T) {
			f, _, record := coordinatorReadFixture(t)
			user := f.user(t, "catalog-viewer", role.role, "catalog@example.test", role.grant, "")
			f.grant(t, user, role.role != "viewer", false)
			first := string(f.seedIssue(t, 1))
			second := string(f.seedIssue(t, 2))
			tools, ctx := coordinatorViewerTools(t, f, record, user)
			want, err := f.service.coordinatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			result, err := tools.handle(t.Context(), runner.AgentToolCall{Name: coordinatorToolFindTools, Arguments: json.RawMessage(`{}`)})
			if err != nil || !result.Success {
				t.Fatalf("find_tools: %#v %v", result, err)
			}
			var listed struct {
				Tools []coordinatorCatalogEntry `json:"tools"`
			}
			if err := json.Unmarshal([]byte(result.Content), &listed); err != nil {
				t.Fatal(err)
			}
			var got, expected []string
			for _, entry := range listed.Tools {
				got = append(got, entry.Name)
			}
			for _, definition := range want {
				expected = append(expected, definition.Name)
			}
			if !slices.Equal(got, expected) || len(got) == 0 {
				t.Fatalf("chat catalog diverged from the operator catalog:\n got %v\nwant %v", got, expected)
			}
			counts[role.name] = len(got)

			result, err = tools.handle(t.Context(), runner.AgentToolCall{Name: coordinatorToolCallTool, Arguments: json.RawMessage(fmt.Sprintf(`{"name":%q,"arguments":{"reference":%q}}`, operatortool.WorkItem, first))})
			if err != nil || !result.Success || !strings.Contains(result.Content, first) {
				t.Fatalf("catalog read: %#v %v", result, err)
			}
			for _, name := range []string{operatortool.WorkList, operatortool.MoveItem, operatortool.ActionResult} {
				result, err = tools.handle(t.Context(), runner.AgentToolCall{Name: coordinatorToolCallTool, Arguments: json.RawMessage(fmt.Sprintf(`{"name":%q}`, name))})
				if err != nil || result.Success {
					t.Fatalf("%s ran through call_tool: %#v %v", name, result, err)
				}
			}

			propose := func(revision string) {
				t.Helper()
				write := fmt.Sprintf(`{"name":%q,"arguments":{"identifier":%q,"expected_revision":%q,"related":%q,"relationship_type":"related","operation":"add"}}`, operatortool.SetDependency, first, revision, second)
				result, err = tools.handle(t.Context(), runner.AgentToolCall{Name: coordinatorToolCallTool, Arguments: json.RawMessage(write)})
				if err != nil {
					t.Fatal(err)
				}
			}
			propose("7")
			if role.role == "viewer" {
				if result.Success {
					t.Fatalf("viewer proposed a write: %s", result.Content)
				}
				return
			}
			if !result.Success || !strings.Contains(result.Content, "proposed") {
				t.Fatalf("write was not proposed: %#v", result)
			}
			response := f.request(t, user, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", coordinatorSettingsProposal(t, f, record.ID))
			requireNativeStatus(t, response, http.StatusConflict)
			if !strings.Contains(response.Body.String(), "current revision is 1") {
				t.Fatalf("stale confirmation did not name the conflict: %s", response.Body.String())
			}
			propose("1")
			messages, err := f.service.conversations.store.listMessages(t.Context(), f.service.database.db, record.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			var action chat.Action
			for _, message := range messages {
				var data struct {
					OperatorAction struct {
						Action chat.Action `json:"action"`
					} `json:"operator_action"`
				}
				if json.Unmarshal(message.Data, &data) == nil && data.OperatorAction.Action.ID != "" {
					action = data.OperatorAction.Action
				}
			}
			if string(action.Kind) != operatortool.SetDependency || action.ProjectID != string(f.project) {
				t.Fatalf("incorrect proposal: %+v", action)
			}
			related := func() string {
				raw, err := json.Marshal(map[string]string{"project_id": string(f.project), "reference": first})
				if err != nil {
					t.Fatal(err)
				}
				read, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkRelationships, Arguments: raw})
				if err != nil {
					t.Fatal(err)
				}
				return string(read.Content)
			}
			if strings.Contains(related(), second) {
				t.Fatal("proposal applied before confirmation")
			}
			requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/conversations/"+record.ID+"/actions", action), http.StatusOK)
			if !strings.Contains(related(), second) {
				t.Fatalf("confirmed catalog write did not apply: %s", related())
			}
			raw, err := json.Marshal(map[string]string{"project_id": string(f.project), "reference": first})
			if err != nil {
				t.Fatal(err)
			}
			history, err := (hostedOperatorExecutor{service: f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkHistory, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(history.Content), `"agent":"Luna"`) {
				t.Fatalf("chat write was not attributed to the agent: %s", history.Content)
			}
		})
	}
	if counts["owner"] <= counts["viewer"] {
		t.Fatalf("catalog did not follow role: %v", counts)
	}
}

func TestCoordinatorTurnOutlivesSharedBrowserAccessToken(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	var skew atomic.Int64
	backend := newFakeCoordinatorBackend()
	f := newHostedSharedFixture(t, func(cfg *Config) {
		cfg.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		cfg.Conversation = &ConversationConfig{Enabled: true, Backend: backend, Workspace: t.TempDir()}
	})
	owner := f.member(t, "owner", "owner", "write")
	seedCoordinatorRepository(t, f.service, f.project)
	for _, test := range []struct {
		name   string
		skew   time.Duration
		access bool
	}{
		{"fresh access token", 0, true},
		{"access token expires during the turn", 2 * time.Minute, true},
		{"turn outlasts its own budget", coordinatorTurnTimeout + time.Minute, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			skew.Store(0)
			results := make(chan runner.AgentToolResult, 1)
			backend.setRun(func(ctx context.Context, _ int, handle runner.AgentToolHandler, _ runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
				skew.Store(int64(test.skew))
				result, err := handle(ctx, runner.AgentToolCall{Name: coordinatorToolFindTools, Arguments: json.RawMessage(`{"query":"dependency"}`)})
				results <- result
				return runner.AgentTurnResult{}, err
			})
			key := "access-" + strings.ReplaceAll(test.name, " ", "-")
			body := fmt.Sprintf(`{"key":%q,"first_message":{"key":%q,"text":"Link #1 and #2 as related"}}`, key, key)
			response := f.serve(t, hostedSharedRequest{user: &owner, method: http.MethodPost, target: f.base + "/conversations", body: body, csrf: cloudassert.CSRFToken("shared-user_owner", "org_security"), mutate: func(c *cloudassert.Claims) { c.AccessExpiresAt = time.Now().Add(time.Minute) }})
			requireNativeStatus(t, response, http.StatusCreated)
			select {
			case result := <-results:
				if result.Success != test.access {
					t.Fatalf("find_tools success = %v, want %v: %s", result.Success, test.access, result.Content)
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("coordinator turn did not run")
			}
		})
	}
}
