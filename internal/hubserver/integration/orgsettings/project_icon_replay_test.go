package orgsettings_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresProjectIconReplay(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{Icons: &memoryProjectIcons{objects: map[string][]byte{}}})
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	primary := hubfixture.NewNative(t, h, h.Primary, "First icon project")
	secondary := hubfixture.NewNative(t, h, hubfixture.Secondary, "Second icon project")
	admin := func(org tracker.OrganizationID) string {
		return hubtest.Credential(t, h.Repository, org, hubtest.CredentialOptions{Scope: "admin", KeyScope: "admin", AllProject: true}).Token
	}
	primaryToken, secondaryToken := admin(primary.Organization()), admin(secondary.Organization())
	for _, test := range []struct {
		project hubfixture.Native
		token   string
		foreign string
	}{
		{primary, primaryToken, secondaryToken},
		{secondary, secondaryToken, primaryToken},
	} {
		t.Run(string(test.project.Organization()), func(t *testing.T) {
			organization, path := test.project.Organization(), test.project.Base
			call := hubfixture.MCP(t, h.Handler(), hubfixture.MCPPath(organization), test.token)
			projectArgs := map[string]any{"project_id": test.project.Project.ID}
			for i, icon := range []projecticon.Icon{{Kind: "emoji", Emoji: "🚀"}, {Kind: "automatic"}, {Kind: "image"}, {Kind: "automatic"}} {
				request := map[string]any{"idempotency_key": fmt.Sprintf("icon-%d", i), "expected_revision": fmt.Sprint(i + 1), "icon": icon}
				if icon.Kind == "image" {
					request["content_base64"] = imageBytes.Bytes()
				}
				response := h.Request(t, http.MethodPut, path+"/icon", test.token, request)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var saved projectstore.Integration
				hubfixture.Decode(t, response, &saved)
				if saved.Revision != tracker.Revision(i+2) || (icon.Kind == "automatic" && saved.Icon != nil) || (icon.Kind != "automatic" && (saved.Icon == nil || saved.Icon.Kind != icon.Kind || saved.Icon.Emoji != icon.Emoji)) {
					t.Fatalf("saved icon = %+v at revision %d", saved.Icon, saved.Revision)
				}
				read := h.Request(t, http.MethodGet, path+"/integration", test.token, nil)
				hubfixture.RequireStatus(t, read, http.StatusOK)
				var current projectstore.Integration
				hubfixture.Decode(t, read, &current)
				if current.Revision != saved.Revision || !reflect.DeepEqual(current.Icon, saved.Icon) {
					t.Fatalf("read icon = %+v at revision %d, want %+v at revision %d", current.Icon, current.Revision, saved.Icon, saved.Revision)
				}
				data := hubfixture.MCPData(t, call("tools/call", "get_project_integration", projectArgs), false)
				var result struct {
					Integration projectstore.Integration `json:"data"`
				}
				if err := json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				if result.Integration.Revision != saved.Revision || !reflect.DeepEqual(result.Integration.Icon, saved.Icon) {
					t.Fatalf("MCP integration = %s", data)
				}
				if organization == h.Primary {
					list := h.Request(t, http.MethodGet, hubfixture.OrganizationBase(organization)+"/projects", h.AdminToken(), nil)
					hubfixture.RequireStatus(t, list, http.StatusOK)
					var projects []struct {
						ID   tracker.ProjectID `json:"id"`
						Icon *projecticon.Icon `json:"project_icon"`
					}
					hubfixture.Decode(t, list, &projects)
					found := false
					for _, project := range projects {
						if project.ID == test.project.Project.ID {
							found = true
							if !reflect.DeepEqual(project.Icon, saved.Icon) {
								t.Fatalf("local project icon = %+v, want %+v", project.Icon, saved.Icon)
							}
						}
					}
					if !found {
						t.Fatal("local projects omitted the updated project")
					}
				}
				replay := h.Request(t, http.MethodPut, path+"/icon", test.token, request)
				hubfixture.RequireStatus(t, replay, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatal("replay did not return original result")
				}
			}

			rpc := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_project_integration", "arguments": projectArgs, "_meta": json.RawMessage(hubfixture.MCPProtocolMeta)}}
			hubfixture.RequireStatus(t, h.WorkCall(t, hubfixture.MCPPath(organization), test.foreign, "get_project_integration", rpc), http.StatusUnauthorized)
			refusedIcon := map[string]any{"idempotency_key": "foreign-icon", "expected_revision": "5", "icon": projecticon.Icon{Kind: "emoji", Emoji: "🚀"}}
			hubfixture.RequireStatus(t, h.Request(t, http.MethodPut, path+"/icon", test.foreign, refusedIcon), http.StatusUnauthorized)
			if organization == h.Primary {
				hubfixture.RequireStatus(t, h.Request(t, http.MethodGet, hubfixture.OrganizationBase(organization)+"/projects", test.foreign, nil), http.StatusUnauthorized)
				freshness := hubfixture.MCPData(t, call("tools/call", "repository_freshness", projectArgs), false)
				var scope struct {
					Organization tracker.OrganizationID `json:"organization_id"`
				}
				if err := json.Unmarshal(freshness, &scope); err != nil || scope.Organization != organization {
					t.Fatalf("freshness response = %s, error = %v", freshness, err)
				}
			}

			content := append(append([]byte{}, imageBytes.Bytes()...), []byte("discarded-metadata")...)
			args := map[string]any{"organization_id": organization, "project_id": test.project.Project.ID, "icon": projecticon.Icon{Kind: "image"}, "content_base64": content, "expected_revision": "5", "request_id": "mcp-icon"}
			first := hubfixture.MCPData(t, call("tools/call", "set_project_icon", args), false)
			replay := hubfixture.MCPData(t, call("tools/call", "set_project_icon", args), false)
			if !bytes.Equal(first, replay) {
				t.Fatalf("MCP replay changed: %s != %s", first, replay)
			}
		})
	}
}
