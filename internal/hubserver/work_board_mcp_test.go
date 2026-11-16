package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/boardview"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkBoardHostedKeysAndMCPParity(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	reporter := f.user(t, "reporter", "reporter", "reporter@example.test", "read", "")
	response := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/projects", map[string]any{"name": "Second project", "states": nativeFixtureStates(), "idempotency_key": "second-project", "grant_access": true})
	requireNativeStatus(t, response, http.StatusCreated)
	var second tracker.NativeProject
	decodeHubResponse(t, response, &second)
	other := f
	other.project = second.ID
	other.grant(t, owner, true, false)
	other.grant(t, reporter, false, false)
	f.seedIssue(t, 1)
	other.seedIssue(t, 1)
	drainTestNativeBoard(t, f.service)
	issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: owner.identity.Hosted, Email: owner.identity.Email}, apikey.HashToken(owner.token))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	key := func(scope hubapi.APIScope, permission apikey.Scope, access hubapi.HostedProjectAccess, projects []string, issuer *hubapi.APICredential) string {
		t.Helper()
		created, err := f.service.createAPITokenFor(t.Context(), hubapi.TokenRequest{Name: "example-agent", Scope: scope, Issuer: issuer, KeyScope: permission, ExpiresAt: &expiry, ProjectIDs: projects, ProjectAccess: access})
		if err != nil {
			t.Fatal(err)
		}
		return created.Token
	}
	readAll := key(hubapi.APIScopeOperator, apikey.ScopeRead, hubapi.HostedProjectsAll, nil, &issuer)
	selected := key(hubapi.APIScopeOperator, apikey.ScopeWrite, hubapi.HostedProjectsSelected, []string{string(f.project)}, &issuer)
	worker := key(hubapi.APIScopeWorker, "", "", nil, nil)
	base := "/api/v2/organizations/org_security/work-board"
	bearer := func(token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		return response
	}
	compact := func(t *testing.T, response *httptest.ResponseRecorder) boardview.CompactBoard {
		t.Helper()
		requireNativeStatus(t, response, http.StatusOK)
		var board boardview.CompactBoard
		decodeHubResponse(t, response, &board)
		return board
	}
	t.Run("read key sees every project", func(t *testing.T) {
		board := compact(t, bearer(readAll, http.MethodGet, base+"?format=compact&lanes=all", ""))
		if len(board.Projects) != 2 || len(board.Cards) != 2 {
			t.Fatalf("projects=%d cards=%d", len(board.Projects), len(board.Cards))
		}
		for _, card := range board.Cards {
			if card.Identifier != boardview.Identifier(card.ProjectID, card.Number) || card.Title == "" || card.State != "Todo" {
				t.Fatalf("card=%+v", card)
			}
		}
	})
	t.Run("selected key asking for two projects", func(t *testing.T) {
		board := compact(t, bearer(selected, http.MethodGet, base+"?format=compact&project="+string(f.project)+"&project="+string(second.ID), ""))
		if len(board.Projects) != 1 || board.Projects[0].ID != f.project || !slices.Equal(board.Unavailable, []tracker.ProjectID{second.ID}) {
			t.Fatalf("projects=%+v unavailable=%v", board.Projects, board.Unavailable)
		}
	})
	t.Run("worker key refused", func(t *testing.T) {
		if response := bearer(worker, http.MethodGet, base, ""); response.Code == http.StatusOK {
			t.Fatalf("worker key read the board: %d", response.Code)
		}
	})
	t.Run("reporter sees only own items", func(t *testing.T) {
		response := f.request(t, reporter, http.MethodGet, base+"?format=compact&lanes=all", nil)
		board := compact(t, response)
		if len(board.Projects) != 2 || len(board.Cards) != 0 {
			t.Fatalf("reporter projects=%d cards=%+v", len(board.Projects), board.Cards)
		}
	})
	t.Run("mcp matches http", func(t *testing.T) {
		captured := make(chan context.Context, 1)
		f.service.echo.POST("/api/v2/organizations/:organization/work-board-authority-test", func(c echo.Context) error {
			captured <- operatortool.BindConnection(c.Request().Context(), "board-connection", "test")
			return c.NoContent(http.StatusOK)
		}, f.service.operatorAuthority)
		requireNativeStatus(t, bearer(readAll, http.MethodPost, "/api/v2/organizations/org_security/work-board-authority-test", `{}`), http.StatusOK)
		ctx := <-captured
		executor := hostedOperatorExecutor{f.service}
		definitions, err := executor.ListTools(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == operatortool.WorkBoard }) {
			t.Fatal("work_board is not listed for a read key")
		}
		for _, test := range []struct{ arguments, query string }{
			{`{"lanes":"all"}`, "format=compact&lanes=all"},
			{`{"lane":["Todo"],"project_ids":["` + string(second.ID) + `"]}`, "format=compact&lane=Todo&project=" + string(second.ID)},
			{`{"lanes":"active","exclude_lane":["Done"]}`, "format=compact&lanes=active&exclude_lane=Done"},
		} {
			result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkBoard, Arguments: json.RawMessage(test.arguments)})
			if err != nil {
				t.Fatalf("%s: %v", test.arguments, err)
			}
			normalize := func(raw []byte) string {
				t.Helper()
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Fatal(err)
				}
				delete(value, "generated_at")
				normalized, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return string(normalized)
			}
			response := bearer(readAll, http.MethodGet, base+"?"+test.query, "")
			requireNativeStatus(t, response, http.StatusOK)
			if mcp, fromHTTP := normalize(result.Content), normalize(response.Body.Bytes()); mcp != fromHTTP {
				t.Fatalf("%s\nmcp=%s\nhttp=%s", test.arguments, mcp, fromHTTP)
			}
		}
		if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkBoard, Arguments: json.RawMessage(`{"lane":["Nonexistent"]}`)}); err == nil {
			t.Fatal("unknown lane accepted over MCP")
		}
	})
}
