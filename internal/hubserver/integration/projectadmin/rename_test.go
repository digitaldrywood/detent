package projectadmin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestRenameProject(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	admin := hubtest.Credential(t, h.Repository, h.Primary, hubtest.CredentialOptions{Scope: "admin", KeyScope: "admin", AllProject: true}).Token
	other := hubfixture.NewNative(t, h, h.Primary, "Unchanged project")
	foreign := hubfixture.NewNative(t, h, hubfixture.Secondary, "Foreign project")
	for _, test := range []struct {
		name, value, revision, target string
		operator                      bool
		status                        int
	}{
		{name: "trimmed", value: "  Renamed project  ", revision: "1", status: http.StatusOK},
		{name: "whitespace", value: " \t\n ", revision: "1", status: http.StatusBadRequest},
		{name: "overlong", value: strings.Repeat("x", 201), revision: "1", status: http.StatusBadRequest},
		{name: "multibyte overlong", value: strings.Repeat("界", 67), revision: "1", status: http.StatusBadRequest},
		{name: "stale", value: "stale-sentinel", revision: "2", status: http.StatusConflict},
		{name: "zero revision", value: "zero-sentinel", revision: "0", status: http.StatusBadRequest},
		{name: "invalid revision encoding", value: "invalid-revision-sentinel", revision: "invalid", status: http.StatusUnprocessableEntity},
		{name: "operator", value: "denied-sentinel", revision: "1", operator: true, status: http.StatusForbidden},
		{name: "unknown", value: "unknown-sentinel", revision: "1", target: "unknown", status: http.StatusNotFound},
		{name: "other organization", value: "foreign-sentinel", revision: "1", target: "foreign", status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewNative(t, h, h.Primary, "Original "+test.name)
			path, token := f.Base, admin
			switch test.target {
			case "unknown":
				path = hubfixture.ProjectBase(h.Primary, "prj_missing")
			case "foreign":
				path = foreign.Base
			}
			if test.operator {
				token = f.Token
			}
			response := h.Request(t, http.MethodPatch, path, token, map[string]string{"name": test.value, "expected_revision": test.revision, "idempotency_key": "rename-" + test.name})
			hubfixture.RequireStatus(t, response, test.status)
			wantName, wantRevision := f.Project.Name, tracker.Revision(1)
			if test.status == http.StatusOK {
				wantName, wantRevision = "Renamed project", 2
			} else if strings.TrimSpace(test.value) != "" && strings.Contains(response.Body.String(), test.value) {
				t.Fatal("error disclosed submitted name")
			}
			requireProjectName(t, f, admin, wantName, wantRevision)
		})
	}
	requireProjectName(t, other, admin, other.Project.Name, 1)
	requireProjectName(t, foreign, h.AdminToken(), foreign.Project.Name, 1)
}

func requireProjectName(t *testing.T, f hubfixture.Native, token, wantName string, wantRevision tracker.Revision) {
	t.Helper()
	response := f.Hub.Request(t, http.MethodGet, f.Base, token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	hubfixture.Decode(t, response, &project)
	if project.Name != wantName || project.Slug != f.Project.Slug {
		t.Fatalf("project name and slug = %q, %q, want %q, %q", project.Name, project.Slug, wantName, f.Project.Slug)
	}
	response = f.Hub.Request(t, http.MethodGet, f.Base+"/integration", token, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var integration projectstore.Integration
	hubfixture.Decode(t, response, &integration)
	if integration.Name != wantName || integration.Revision != wantRevision {
		t.Fatalf("integration name and revision = %q, %d, want %q, %d", integration.Name, integration.Revision, wantName, wantRevision)
	}
}

func TestRenameProjectMCP(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	admin := f.Hub.AdminToken()
	call := func(t *testing.T, args operatortool.RenameProjectArguments, conflict bool) json.RawMessage {
		t.Helper()
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "rename_project", "arguments": args, "_meta": json.RawMessage(hubfixture.MCPProtocolMeta)}}
		response := f.Hub.WorkCall(t, hubfixture.MCPPath(f.Organization()), admin, "rename_project", body)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var envelope struct {
			Result struct {
				Data    json.RawMessage `json:"structuredContent"`
				IsError bool            `json:"isError"`
			} `json:"result"`
		}
		hubfixture.Decode(t, response, &envelope)
		if envelope.Result.IsError != conflict || len(envelope.Result.Data) == 0 {
			t.Fatalf("MCP response: %s", response.Body.String())
		}
		return envelope.Result.Data
	}
	args := operatortool.RenameProjectArguments{OrganizationID: string(f.Organization()), ProjectID: string(f.Project.ID), Name: "Tool name", ExpectedRevision: 1, RequestID: "rename"}
	for _, name := range []string{"success", "replay"} {
		t.Run(name, func(t *testing.T) {
			var action chatpkg.Action
			if err := json.Unmarshal(call(t, args, false), &action); err != nil {
				t.Fatal(err)
			}
			var integration projectstore.Integration
			if err := json.Unmarshal([]byte(action.Result), &integration); err != nil {
				t.Fatalf("action %+v: %v", action, err)
			}
			if integration.Name != args.Name || integration.Revision != 2 {
				t.Fatalf("rename = %+v", integration)
			}
			replay := f.Hub.Request(t, http.MethodPatch, f.Base, admin, map[string]string{"name": args.Name, "expected_revision": "1", "idempotency_key": args.RequestID})
			hubfixture.RequireStatus(t, replay, http.StatusOK)
			if strings.TrimSpace(replay.Body.String()) != action.Result {
				t.Fatal("API and MCP replay differ")
			}
		})
	}
	args.RequestID = "stale"
	var conflict operatortool.ConflictError
	if err := json.Unmarshal(call(t, args, true), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Code != "revision_conflict" || conflict.CurrentRevision != 2 {
		t.Fatalf("conflict = %+v", conflict)
	}
	requireProjectName(t, f, admin, args.Name, 2)
}

func TestHostedProjectRename(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	owner := h.User(t, "owner", "owner", "owner@example.test", "", "")
	member := h.User(t, "member", "member", "member@example.test", "", "")
	for _, test := range []struct {
		name, revision string
		status         int
	}{
		{"owner", "1", http.StatusOK},
		{"invalid", "0", http.StatusBadRequest},
		{"stale", "999", http.StatusConflict},
		{"member", "1", http.StatusForbidden},
		{"unknown", "1", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := h.SeedProject(t, tracker.ProjectID("prj_rename_"+test.name), "Original "+test.name)
			target, actor := project.ID, owner
			switch test.name {
			case "member":
				h.Grant(t, member, project.ID, true, false)
				actor = member
			case "unknown":
				target = "prj_missing"
			}
			path := hubfixture.ProjectBase(h.Primary, target)
			response := h.Request(t, actor, http.MethodPatch, path, map[string]string{"name": "Hosted rename", "expected_revision": test.revision, "idempotency_key": "hosted-rename-" + test.name})
			hubfixture.RequireStatus(t, response, test.status)
			if test.status != http.StatusOK {
				if strings.Contains(response.Body.String(), "Hosted rename") {
					t.Fatal("submitted name leaked")
				}
				return
			}
			var audited bool
			if err := h.Admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM hosted_audit WHERE organization_id=$1 AND project_id=$2 AND route=$3 AND status=200)", h.Primary, project.ID, "PATCH /api/v2/organizations/:organization/projects/:project").Scan(&audited); err != nil {
				t.Fatal(err)
			}
			if !audited {
				t.Fatal("rename audit missing")
			}
		})
	}
}
