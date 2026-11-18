package githubbinding_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBindNativeRepositoryToolReceipts(t *testing.T) {
	for _, source := range []string{"", "runner_checkout"} {
		t.Run("source="+source, func(t *testing.T) {
			t.Parallel()
			backend := &scriptedReconcileBackend{}
			for _, name := range []string{"first-project", "second-project"} {
				backend.steps = append(backend.steps, reconcileStep{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: "R_" + name, Owner: "example-org", Name: name, UpdatedAt: time.Now().UTC()}}})
			}
			fixture := newBindingFixture(t, hubserver.Config{ReconcileBackend: backend})
			service := fixture.service
			first := fixture.project(t, "first-bind")
			second := fixture.project(t, "second-bind")
			path := "/api/v2/organizations/" + string(first.project.OrganizationID) + "/mcp"
			callTool := func(t *testing.T, call operatortool.Call, conflict bool) chatpkg.Action {
				t.Helper()
				response := performHubWorkCall(t, service, path, testHubAdminToken, call.Name, map[string]any{
					"jsonrpc": "2.0", "id": 1, "method": "tools/call",
					"params": map[string]any{"name": call.Name, "arguments": call.Arguments, "_meta": json.RawMessage(fleetProtocolMeta)},
				})
				requireNativeStatus(t, response, http.StatusOK)
				var envelope struct {
					Result struct {
						Data    json.RawMessage `json:"structuredContent"`
						IsError bool            `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &envelope)
				if envelope.Result.IsError != conflict {
					t.Fatalf("tool response=%s", response.Body.String())
				}
				if conflict {
					if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Text != operatortool.ErrInvalidArguments.Error() {
						t.Fatalf("changed retry=%s", response.Body.String())
					}
					return chatpkg.Action{}
				}
				var action chatpkg.Action
				if err := json.Unmarshal(envelope.Result.Data, &action); err != nil || action.Status != chatpkg.ActionSucceeded {
					t.Fatalf("bind action=%+v, error=%v", action, err)
				}
				return action
			}
			input := operatortool.RepositoryInput{ExpectedRevision: 1, Repository: "example-org/first-project", Source: source}
			call := projectCall(t, "bind_native_repository", string(first.project.ID), "first-bind", input)
			bound := callTool(t, call, false)
			restPath := first.base + "/integration/repository"
			if source == "runner_checkout" {
				restPath = first.base + "/onboarding/repository"
			}
			restInput := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "first-bind"}, ExpectedRevision: 1, Repository: input.Repository, Source: source}
			replayed := performHubAPIRequest(t, service, http.MethodPost, restPath, testHubAdminToken, restInput)
			requireNativeStatus(t, replayed, http.StatusOK)
			if strings.TrimSpace(replayed.Body.String()) != bound.Result {
				t.Fatal("REST replay changed the MCP receipt")
			}
			restInput.Repository = "example-org/changed-project"
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, restPath, testHubAdminToken, restInput), http.StatusConflict)
			for _, test := range []struct {
				name       string
				project    tracker.ProjectID
				key        string
				repository string
				conflict   bool
				replay     bool
			}{
				{"same key and content", first.project.ID, "first-bind", input.Repository, false, true},
				{"same key and different content", first.project.ID, "first-bind", "example-org/changed-project", true, false},
				{"different key and content", second.project.ID, "second-bind", "example-org/second-project", false, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					testInput := input
					testInput.Repository = test.repository
					call := projectCall(t, "bind_native_repository", string(test.project), test.key, testInput)
					action := callTool(t, call, test.conflict)
					if test.conflict {
						return
					}
					var integration ProjectIntegration
					if err := json.Unmarshal([]byte(action.Result), &integration); err != nil {
						t.Fatal(err)
					}
					repository := integration.Repository
					if source == "runner_checkout" {
						repository = integration.CheckoutRepository
					}
					if repository != test.repository || integration.Revision != 2 {
						t.Fatalf("integration=%+v", integration)
					}
					if test.replay && action.Result != bound.Result {
						t.Fatal("retry changed the original result")
					}
				})
			}
			for _, fixture := range []nativeFixture{first, second} {
				response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/integration", testHubAdminToken, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var integration ProjectIntegration
				decodeHubResponse(t, response, &integration)
				repository := integration.Repository
				if source == "runner_checkout" {
					repository = integration.CheckoutRepository
				}
				want := "example-org/first-project"
				if fixture.project.ID == second.project.ID {
					want = "example-org/second-project"
				}
				if repository != want || integration.Revision != 2 {
					t.Fatalf("persisted binding=%+v, want repository %s at revision 2", integration, want)
				}
			}
			wantFetches := 2
			if source == "runner_checkout" {
				wantFetches = 0
			}
			if got := len(backend.Requests()); got != wantFetches {
				t.Fatalf("provider fetches=%d, want %d", got, wantFetches)
			}
		})
	}
}
