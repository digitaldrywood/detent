package hubserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRenameProject(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, value, revision, target string
		operator                      bool
		status                        int
	}{
		{name: "trimmed", value: "  Renamed project  ", revision: "1", status: 200},
		{name: "empty", revision: "1", status: 400},
		{name: "whitespace", value: " \t\n ", revision: "1", status: 400},
		{name: "overlong", value: strings.Repeat("x", 201), revision: "1", status: 400},
		{name: "multibyte overlong", value: strings.Repeat("界", 67), revision: "1", status: 400},
		{name: "stale", value: "stale-sentinel", revision: "2", status: 409},
		{name: "zero revision", value: "zero-sentinel", revision: "0", status: 400},
		{name: "operator", value: "denied-sentinel", revision: "1", operator: true, status: 403},
		{name: "unknown", value: "unknown-sentinel", revision: "1", target: "unknown", status: 404},
		{name: "other organization", value: "foreign-sentinel", revision: "1", target: "foreign", status: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "original")
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "second")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			before, err := readProjectIntegration(t.Context(), f.service.database.reader, scope)
			if err != nil {
				t.Fatal(err)
			}
			path := f.base
			if test.target == "unknown" {
				path = strings.TrimSuffix(f.base, string(f.project.ID)) + "prj_missing"
			}
			if test.target == "foreign" {
				path = strings.Replace(f.base, string(f.project.OrganizationID), "org_other", 1)
			}
			token := testHubAdminToken
			if test.operator {
				token = f.token
			}
			response := performHubAPIRequest(t, f.service, http.MethodPatch, path, token, map[string]string{"name": test.value, "expected_revision": test.revision, "idempotency_key": "rename"})
			requireNativeStatus(t, response, test.status)
			if test.status != 200 && strings.TrimSpace(test.value) != "" && strings.Contains(response.Body.String(), test.value) {
				t.Fatal("error disclosed submitted name")
			}
			after, err := readProjectIntegration(t.Context(), f.service.database.reader, scope)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			if test.status == 200 {
				want.Name = "Renamed project"
				want.Revision++
			}
			if !reflect.DeepEqual(after, want) {
				t.Fatalf("integration changed unexpectedly: got %+v, want %+v", after, want)
			}
			project, err := readNativeProject(t.Context(), f.service.database.reader, scope)
			if err != nil {
				t.Fatal(err)
			}
			if project.Slug != f.project.Slug {
				t.Fatal("rename changed slug")
			}
			second, err := readNativeProject(t.Context(), f.service.database.reader, nativeScope{organization: other.project.OrganizationID, project: other.project.ID})
			if err != nil || second.Name != "second" {
				t.Fatalf("other project = %+v, %v", second, err)
			}
		})
	}
}

func TestRenameProjectMCP(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "rename-tool")
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	for _, test := range []struct {
		name, requestID string
		revision        tracker.Revision
		conflict        bool
	}{
		{"success", "rename", 1, false},
		{"replay", "rename", 1, false},
		{"conflict", "stale", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := operatortool.RenameProjectArguments{OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID), Name: "Tool name", ExpectedRevision: test.revision, RequestID: test.requestID}
			response := performHubWorkCall(t, f.service, path, testHubAdminToken, "rename_project", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "rename_project", "arguments": args, "_meta": json.RawMessage(fleetProtocolMeta)}})
			requireNativeStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					Data    json.RawMessage `json:"structuredContent"`
					IsError bool            `json:"isError"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			if envelope.Result.IsError != test.conflict {
				t.Fatalf("MCP response: %s", response.Body.String())
			}
			if test.conflict {
				var failure operatortool.ConflictError
				if err := json.Unmarshal(envelope.Result.Data, &failure); err != nil {
					t.Fatalf("MCP conflict response: %s: %v", response.Body.String(), err)
				}
				if failure.Code != "revision_conflict" || failure.CurrentRevision != 2 {
					t.Fatalf("conflict = %+v", failure)
				}
				current, err := readProjectIntegration(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
				if err != nil {
					t.Fatal(err)
				}
				if current.Name != "Tool name" || current.Revision != 2 {
					t.Fatalf("conflicting rename changed project: %+v", current)
				}
				return
			}
			var action chatpkg.Action
			if err := json.Unmarshal(envelope.Result.Data, &action); err != nil {
				t.Fatal(err)
			}
			var integration ProjectIntegration
			if err := json.Unmarshal([]byte(action.Result), &integration); err != nil {
				t.Fatalf("action %+v: %v", action, err)
			}
			if integration.Name != "Tool name" || integration.Revision != 2 {
				t.Fatalf("rename = %+v", integration)
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPatch, f.base, testHubAdminToken, renameProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: "rename"}, Name: args.Name, ExpectedRevision: 1})
			requireNativeStatus(t, replay, http.StatusOK)
			if strings.TrimSpace(replay.Body.String()) != action.Result {
				t.Fatal("API and MCP replay differ")
			}
		})
	}
}

func TestHostedProjectRename(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, target, revision string
		status                          int
	}{
		{"owner", "owner", "", "", 200},
		{"invalid", "owner", "", "0", 400},
		{"stale", "owner", "", "999", 409},
		{"member", "owner", "", "", 403},
		{"unknown", "owner", "prj_missing", "", 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
			before, err := readProjectIntegration(t.Context(), f.service.database.reader, scope)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "member" {
				if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_owner", "member"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role='member' WHERE user_id='user_browser_owner'"); err != nil {
					t.Fatal(err)
				}
			}
			target := f.project
			if test.target != "" {
				target = test.target
			}
			revision := before.Revision
			if test.revision == "0" {
				revision = 0
			}
			if test.revision == "999" {
				revision = 999
			}
			body, err := json.Marshal(renameProjectRequest{Mutation: tracker.Mutation{IdempotencyKey: "hosted-rename"}, Name: "Hosted rename", ExpectedRevision: revision})
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies[test.account].Value)}
			response := f.rawAPI(t, test.account, http.MethodPatch, browserHostedOrganizationBase+"/projects/"+target, string(body), headers)
			requireNativeStatus(t, response, test.status)
			if test.status != 200 && strings.Contains(response.Body.String(), "Hosted rename") {
				t.Fatal("submitted name leaked")
			}
			if test.status == 200 {
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE project_id=? AND route=? AND status=200", f.project, "PATCH /api/v2/organizations/:organization/projects/:project").Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count == 0 {
					t.Fatal("rename audit missing")
				}
			}
		})
	}
}
