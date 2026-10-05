package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func projectCall[T any](t *testing.T, name, project, key string, input T) operatortool.Call {
	t.Helper()
	raw, err := json.Marshal(operatortool.ProjectRequest[T]{ProjectID: project, RequestID: key, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return operatortool.Call{Name: name, Arguments: raw}
}
func projectAction(t *testing.T, e hubProjectExecutor, ctx context.Context, call operatortool.Call) chatpkg.Action {
	t.Helper()
	result, err := e.Execute(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var action chatpkg.Action
	if err := json.Unmarshal(result.Content, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

// Import page retries must reuse the durable receipt and never expose transport errors.
func TestProjectImportToolReceipts(t *testing.T) {
	backend := &importFixtureBackend{}
	f := newIntegrationFixture(t, backend)
	captureds := make(chan context.Context, 1)
	// The resolver derives the organization from the registered native entry path.
	f.service.echo.GET("/api/v2/organizations/:organization/project-import-test", func(c echo.Context) error { captureds <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/project-import-test"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, path, testHubAdminToken, nil), http.StatusOK)
	captured := <-captureds
	e := hubProjectExecutor{f.service}
	connect := func(id string) context.Context {
		ctx := operatortool.BindConnection(captured, id, "import fixture")
		if err := e.OpenConnection(ctx); err != nil {
			t.Fatal(err)
		}
		return ctx
	}
	ctx := connect("import")
	create := projectCall(t, "create_native_project", "", "create-native", operatortool.ProjectCreateInput{Name: "tool project", States: nativeFixtureStates()})
	created := projectAction(t, e, ctx, create)
	if created.Status != chatpkg.ActionSucceeded {
		t.Fatalf("create native=%+v", created)
	}
	if replay := projectAction(t, e, connect("create-native-reconnect"), create); replay.Result != created.Result {
		t.Fatalf("native create replay=%+v", replay)
	}
	// An unhosted hub has no authenticated browser approval surface.
	if _, err := e.Execute(ctx, projectCall(t, "revoke_project_policy", string(f.project.ID), "no-browser", operatortool.PolicyRevokeInput{ExpectedID: hubTestPolicy().ID})); !errors.Is(err, errProjectServiceUnavailable) {
		t.Fatalf("absent approval surface=%v", err)
	}
	approveHubTestPolicy(t, f.service, "/api/v1/repositories/digitaldrywood/detent/policy", hubTestPolicy())
	legacy, err := e.Execute(ctx, operatortool.Call{Name: "get_project_policy", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","repository_policy":true}`)})
	if err != nil || !strings.Contains(string(legacy.Content), hubTestPolicy().ID) {
		t.Fatalf("repository policy=%s %v", legacy.Content, err)
	}

	start := projectCall(t, "start_git_hub_import", string(f.project.ID), "start-tool", operatortool.ImportStartInput{IssueNumber: 1})
	a := projectAction(t, e, ctx, start)
	if a.Status != chatpkg.ActionSucceeded {
		t.Fatalf("ordinary import=%+v", a)
	}
	var job GitHubImport
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	advance := func(ctx context.Context, key string) chatpkg.Action {
		return projectAction(t, e, ctx, projectCall(t, "advance_git_hub_import", string(f.project.ID), key, operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision}))
	}
	a = advance(ctx, "page-one")
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	if job.Stage != "comments" {
		t.Fatalf("stage=%s", job.Stage)
	}
	call := projectCall(t, "advance_git_hub_import", string(f.project.ID), "page-two", operatortool.ImportAdvanceInput{ImportID: job.ID, ExpectedRevision: job.Revision})
	a = projectAction(t, e, ctx, call)
	requests := len(backend.requests)
	if replay := projectAction(t, e, connect("import-reconnected"), call); replay.Result != a.Result || len(backend.requests) != requests {
		t.Fatalf("replay repeated import fetch: %+v", replay)
	}
	if err := json.Unmarshal([]byte(a.Result), &job); err != nil {
		t.Fatal(err)
	}
	backend.failPage = true
	a = advance(ctx, "failed-page")
	if strings.Contains(a.Result, "GitHub unavailable") || !strings.Contains(a.Result, "Import transport is unavailable") {
		t.Fatalf("raw error escaped: %s", a.Result)
	}
	read := operatortool.Call{Name: "get_git_hub_import", Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `","import_id":"` + job.ID + `"}`)}
	result, err := e.Execute(ctx, read)
	if err != nil || strings.Contains(string(result.Content), "GitHub unavailable") {
		t.Fatalf("read redaction=%s %v", result.Content, err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-import-project")
	read.Arguments = json.RawMessage(`{"project_id":"` + string(other.project.ID) + `","import_id":"` + job.ID + `"}`)
	if _, err := e.Execute(ctx, read); err == nil {
		t.Fatal("foreign project import disclosed")
	}
}
