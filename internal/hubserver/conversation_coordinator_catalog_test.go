package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
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
