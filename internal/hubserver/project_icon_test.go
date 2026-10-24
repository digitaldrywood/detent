package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type memoryProjectIcons struct {
	objects  map[string][]byte
	afterPut func(context.Context) error
}

func (m *memoryProjectIcons) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	m.objects[organization+"/"+id] = append([]byte{}, content...)
	if m.afterPut != nil {
		return m.afterPut(ctx)
	}
	return nil
}

func (m *memoryProjectIcons) DeleteIcon(ctx context.Context, organization, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(m.objects, organization+"/"+id)
	return nil
}

func TestProjectIconLifecycle(t *testing.T) {
	t.Parallel()
	store := &memoryProjectIcons{objects: map[string][]byte{}}
	f := newDefaultNativeFixture(t, Config{Icons: store})
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other project")
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	var previousImage string
	revision := 1
	for index, icon := range []projecticon.Icon{
		{Kind: "lucide", Name: "rocket", Color: "emerald"}, {Kind: "lucide", Name: "cpu", Color: "red"}, {Kind: "automatic"},
		{Kind: "emoji", Emoji: "🚀"}, {Kind: "emoji", Emoji: "👨‍👩‍👧‍👦"}, {Kind: "automatic"},
		{Kind: "image"}, {Kind: "image"}, {Kind: "automatic"},
	} {
		t.Run(fmt.Sprintf("%d %s", index, icon.Kind), func(t *testing.T) {
			body := map[string]any{"icon": icon, "expected_revision": strconv.Itoa(revision), "idempotency_key": fmt.Sprintf("icon-%d", index)}
			if icon.Kind == "image" {
				body["content_base64"] = buffer.Bytes()
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", testHubAdminToken, body)
			requireNativeStatus(t, response, http.StatusOK)
			var saved ProjectIntegration
			decodeHubResponse(t, response, &saved)
			revision++
			if int(saved.Revision) != revision {
				t.Fatalf("revision=%d want %d", saved.Revision, revision)
			}
			if icon.Kind == "automatic" {
				if saved.Icon != nil {
					t.Fatalf("reset = %+v", saved.Icon)
				}
			} else if saved.Icon == nil || saved.Icon.Kind != icon.Kind || saved.Icon.Name != icon.Name || saved.Icon.Color != icon.Color || saved.Icon.Emoji != icon.Emoji {
				t.Fatalf("saved icon = %+v", saved.Icon)
			}
			if previousImage != "" {
				var deleted bool
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT deleted_at IS NOT NULL FROM attachments WHERE id=?", previousImage).Scan(&deleted); err != nil || !deleted {
					t.Fatalf("replacement did not tombstone old object: %v", err)
				}
			}
			previousImage = ""
			if saved.Icon != nil && saved.Icon.Kind == "image" {
				previousImage = saved.Icon.ImageKey
				content := store.objects[string(f.project.OrganizationID)+"/"+previousImage]
				config, err := png.DecodeConfig(bytes.NewReader(content))
				if err != nil || config.Width != 128 || config.Height != 128 || saved.Icon.ImageSHA256 == "" || saved.Icon.URL == "" {
					t.Fatalf("stored icon=%+v, %+v, %v", saved.Icon, config, err)
				}
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", testHubAdminToken, body)
			requireNativeStatus(t, replay, http.StatusOK)
			if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
				t.Fatal("replay differs")
			}
			second, err := readProjectIntegration(t.Context(), f.service.database.reader, nativeScope{organization: other.project.OrganizationID, project: other.project.ID})
			if err != nil || second.Icon != nil || second.Revision != 1 {
				t.Fatalf("other project=%+v, %v", second, err)
			}
		})
	}
}

func TestSetProjectIconRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		icon     projecticon.Icon
		revision string
		operator bool
		status   int
	}{
		{"stale", projecticon.Icon{Kind: "emoji", Emoji: "🚀"}, "2", false, 409},
		{"zero", projecticon.Icon{Kind: "automatic"}, "0", false, 422},
		{"operator", projecticon.Icon{Kind: "emoji", Emoji: "🚀"}, "1", true, 403},
		{"unknown icon", projecticon.Icon{Kind: "lucide", Name: "does-not-exist", Color: "blue"}, "1", false, 422},
		{"unknown color", projecticon.Icon{Kind: "lucide", Name: "rocket", Color: "no-color"}, "1", false, 422},
		{"two graphemes", projecticon.Icon{Kind: "emoji", Emoji: "🚀🌍"}, "1", false, 422},
		{"unvalidated image", projecticon.Icon{Kind: "image", ImageKey: "att_other"}, "1", false, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "icon permissions")
			token := testHubAdminToken
			if test.operator {
				token = f.token
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", token, map[string]any{"icon": test.icon, "expected_revision": test.revision, "idempotency_key": "icon"})
			requireNativeStatus(t, response, test.status)
			current, err := readProjectIntegration(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil || current.Icon != nil || current.Revision != 1 {
				t.Fatalf("refused update changed project: %+v %v", current, err)
			}
		})
	}
}

func TestProjectIconMCP(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "icon tool")
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	response := performHubWorkCall(t, f.service, path, testHubAdminToken, "set_project_icon", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "set_project_icon", "arguments": map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID, "icon": projecticon.Icon{Kind: "emoji", Emoji: "🚀"}, "expected_revision": "1", "request_id": "tool-icon"}, "_meta": json.RawMessage(fleetProtocolMeta)}})
	if response.Code != 200 {
		t.Fatalf("MCP = %s", response.Body.String())
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	decodeHubResponse(t, response, &envelope)
	if envelope.Result.IsError {
		t.Fatal(response.Body.String())
	}
	saved, err := readProjectIntegration(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil || saved.Icon == nil || saved.Icon.Emoji != "🚀" || saved.Revision != 2 {
		t.Fatalf("MCP icon = %+v %v", saved, err)
	}
}

func TestProjectIconMCPImage(t *testing.T) {
	t.Parallel()
	store := &memoryProjectIcons{objects: map[string][]byte{}}
	f := newDefaultNativeFixture(t, Config{Icons: store})
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	state := uint32(1234567)
	for i := range img.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i] = byte(state)
	}
	var content bytes.Buffer
	if err := png.Encode(&content, img); err != nil {
		t.Fatal(err)
	}
	metadata := []byte("private-location-sentinel")
	input := append(append([]byte{}, content.Bytes()...), metadata...)
	args := operatortool.SetProjectIconArguments{OrganizationID: string(f.project.OrganizationID), ProjectID: string(f.project.ID), Icon: projecticon.Icon{Kind: "image"}, Content: input, ExpectedRevision: 1, RequestID: "mcp-image"}
	response := performHubWorkCall(t, f.service, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", testHubAdminToken, "set_project_icon", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "set_project_icon", "arguments": args, "_meta": json.RawMessage(fleetProtocolMeta)}})
	var envelope struct {
		Result struct {
			Data    json.RawMessage `json:"structuredContent"`
			IsError bool            `json:"isError"`
		} `json:"result"`
	}
	decodeHubResponse(t, response, &envelope)
	if envelope.Result.IsError || len(envelope.Result.Data) == 0 {
		t.Fatal(response.Body.String())
	}
	var action chatpkg.Action
	if err := json.Unmarshal(envelope.Result.Data, &action); err != nil {
		t.Fatal(err)
	}
	if len(action.Arguments) <= operatortool.MaxArgumentBytes {
		t.Fatal("fixture did not exercise the image argument budget")
	}
	var stored operatortool.ProjectRequest[operatortool.SetProjectIconInput]
	if err := json.Unmarshal(action.Arguments, &stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Input.Content, metadata) || len(store.objects) != 1 {
		t.Fatal("original metadata retained or image was not stored")
	}
}

func TestHostedProjectIcon(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, account, revision string
		status                  int
	}{
		{"owner", "owner", "", 200}, {"member", "owner", "", 403}, {"viewer", "viewer", "", 404}, {"stale", "owner", "999", 409},
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
			revision := fmt.Sprint(before.Revision)
			if test.revision != "" {
				revision = test.revision
			}
			body := fmt.Sprintf(`{"icon":{"kind":"emoji","emoji":"🚀"},"expected_revision":%q,"idempotency_key":"hosted-icon"}`, revision)
			response := f.rawAPI(t, test.account, http.MethodPut, browserHostedOrganizationBase+"/projects/"+f.project+"/icon", body, map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies[test.account].Value)})
			requireNativeStatus(t, response, test.status)
			if test.status == 200 {
				projects, err := f.service.hostedReadableProjects(t.Context(), apiCredential{Hosted: &auth.HostedIdentity{Subject: "user_browser_owner"}, HostedRole: "owner"})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, p := range projects {
					if p.ID == f.project && p.Icon != nil && p.Icon.Emoji == "🚀" {
						found = true
					}
				}
				if !found {
					t.Fatalf("saved icon missing from bootstrap: %+v", projects)
				}
				if !strings.Contains(response.Body.String(), `"project_icon":{"kind":"emoji"`) {
					t.Fatal(response.Body.String())
				}
			} else {
				after, err := readProjectIntegration(t.Context(), f.service.database.reader, scope)
				if err != nil || after.Icon != nil || after.Revision != before.Revision {
					t.Fatalf("refusal changed project: %+v %v", after, err)
				}
			}
		})
	}
}
