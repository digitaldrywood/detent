package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOperatorChatSessions(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository}, config: Config{TenantRepository: repository, AccessRepository: repository, Logger: discardLogger(), now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedStartupOrganization(t, repository, org)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin", Name: "Example", Hash: apikey.HashToken(string(org)), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "project", Name: string(org), Profile: "native", CreatedAt: now}, States: nativeFixtureStates()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for pass := 0; pass < 2; pass++ {
		service.registerOperatorTools(service.echo)
		for _, org := range []string{"org_first", "org_second"} {
			call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_project_integration", "arguments": map[string]any{"project_id": "project"}, "_meta": json.RawMessage(fleetProtocolMeta)}}
			response := performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", org, "get_project_integration", call)
			requireNativeStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					IsError bool `json:"isError"`
					Data    struct {
						Organization string             `json:"organization_id"`
						Integration  ProjectIntegration `json:"data"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			if envelope.Result.IsError || envelope.Result.Data.Organization != org || envelope.Result.Data.Integration.Name != org {
				t.Fatalf("MCP=%s", response.Body.String())
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM operator_chat_sessions WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 1 {
				t.Fatalf("persisted sessions=%d err=%v", count, err)
			}
			foreign := "org_first"
			if org == foreign {
				foreign = "org_second"
			}
			requireNativeStatus(t, performHubWorkCall(t, service, "/api/v2/organizations/"+org+"/mcp", foreign, "get_project_integration", call), http.StatusUnauthorized)
		}
	}
	store := operatorChatStore{service.database}
	for _, org := range []string{"org_first", "org_second"} {
		identity := operatortool.Identity{OrganizationID: org, PrincipalID: "admin", CredentialID: apikey.HashToken(org)}
		ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity})
		state := chat.SessionState{ID: "shared", Identity: identity, Client: "example", LastUsedAt: now, Origin: `{"key_id":"example-key","credential_binding":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","subject":"admin","issuer":"entry","generation":1}`}
		if err := store.Save(ctx, state, now, time.Hour, 128); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			at    time.Time
			found bool
		}{{now, true}, {now.Add(time.Hour), true}, {now.Add(time.Hour + time.Microsecond), false}} {
			got, found, err := store.Load(ctx, state.ID, test.at, time.Hour)
			if err != nil || found != test.found || found && (got.Identity != identity || got.Client != "example" || got.Origin != state.Origin) {
				t.Fatalf("load=%+v found=%v err=%v", got, found, err)
			}
		}
		restoredContext := context.WithValue(t.Context(), operatorChatOrganizationKey{}, tracker.OrganizationID(org))
		if restored, found, err := store.Load(restoredContext, state.ID, now, time.Hour); err != nil || !found || restored.Identity != identity {
			t.Fatalf("coordinator restore=%+v %v %v", restored, found, err)
		}
		changed := state
		changed.Identity.PrincipalID = "different"
		if err := store.Save(ctx, changed, now, time.Hour, 128); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("identity overwrite=%v", err)
		}
		restored, found, err := store.Load(ctx, state.ID, now, time.Hour)
		if err != nil || !found || restored.Identity != identity {
			t.Fatalf("identity after refusal=%+v %v %v", restored, found, err)
		}
		state.ID = "newest"
		state.LastUsedAt = now.Add(time.Second)
		if err := store.Save(ctx, state, now, time.Hour, 2); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM operator_chat_sessions WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 2 {
			t.Fatalf("bounded sessions=%d err=%v", count, err)
		}
		if _, found, err := store.Load(ctx, "newest", now, time.Hour); err != nil || !found {
			t.Fatalf("new session missing: %v", err)
		}
	}

}
