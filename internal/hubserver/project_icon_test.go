package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	chatpkg "github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type memoryProjectIcons struct {
	objects   map[string][]byte
	afterPut  func(context.Context) error
	beforePut func(string, string, []byte)
}

func (m *memoryProjectIcons) PutIcon(ctx context.Context, organization, id string, content []byte) error {
	if m.beforePut != nil {
		m.beforePut(organization, id, content)
	}
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
				stored, err := readProjectIconAttachment(t.Context(), f.service, f.project.OrganizationID, f.project.ID, previousImage)
				if err != nil || stored.Metadata.DeletedAt == nil {
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
				stored, err := readProjectIconAttachment(t.Context(), f.service, f.project.OrganizationID, f.project.ID, previousImage)
				retention := stored.Retention
				if err != nil || retention != "organization" {
					t.Fatalf("committed icon retention=%q, %v", retention, err)
				}
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", testHubAdminToken, body)
			requireNativeStatus(t, replay, http.StatusOK)
			if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
				t.Fatal("replay differs")
			}
			second, err := readProjectIconIntegration(t.Context(), f.service, hubscope.NativeScope{Organization: other.project.OrganizationID, Project: other.project.ID})
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
		failure  string
	}{
		{"stale", projecticon.Icon{Kind: "emoji", Emoji: "🚀"}, "2", false, 409, ""},
		{"zero", projecticon.Icon{Kind: "automatic"}, "0", false, 422, ""},
		{"operator", projecticon.Icon{Kind: "emoji", Emoji: "🚀"}, "1", true, 403, ""},
		{"unknown icon", projecticon.Icon{Kind: "lucide", Name: "does-not-exist", Color: "blue"}, "1", false, 422, ""},
		{"unknown color", projecticon.Icon{Kind: "lucide", Name: "rocket", Color: "no-color"}, "1", false, 422, ""},
		{"two graphemes", projecticon.Icon{Kind: "emoji", Emoji: "🚀🌍"}, "1", false, 422, ""},
		{"unvalidated image", projecticon.Icon{Kind: "image", ImageKey: "att_other"}, "1", false, 422, ""},
		{"image success", projecticon.Icon{Kind: "image"}, "1", false, 200, ""},
		{"quota", projecticon.Icon{Kind: "image"}, "1", false, 429, "quota"},
		{"raced revision", projecticon.Icon{Kind: "image"}, "1", false, 409, "revision"},
		{"raced replay", projecticon.Icon{Kind: "image"}, "1", false, 200, "replay"},
		{"upload deadline", projecticon.Icon{Kind: "image"}, "1", false, 500, "deadline"},
		{"store then error", projecticon.Icon{Kind: "image"}, "1", false, 500, "upload"},
		{"tombstone failure", projecticon.Icon{Kind: "image"}, "1", false, 500, "tombstone"},
		{"mutation failure", projecticon.Icon{Kind: "image"}, "1", false, 500, "mutation"},
		{"cancelled mutation", projecticon.Icon{Kind: "image"}, "1", false, 500, "cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryProjectIcons{objects: map[string][]byte{}}
			f := newDefaultNativeFixture(t, Config{Icons: store})
			token := testHubAdminToken
			if test.operator {
				token = f.token
			}
			body := map[string]any{"icon": test.icon, "expected_revision": test.revision, "idempotency_key": "icon"}
			if test.icon.Kind == "image" {
				var content bytes.Buffer
				if err := png.Encode(&content, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
					t.Fatal(err)
				}
				body["content_base64"] = content.Bytes()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch test.failure {
			case "quota":
				plans := hostedplans.PilotPlans()
				plans.Plans[0].Allowances["collaboration_bytes"] = 1
				f.service.database.hostedOrganization = f.project.OrganizationID
				if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
					t.Fatal(err)
				}
			case "mutation":
				installProjectIconFailure(t, f, "mutation")
			case "deadline":
				store.afterPut = func(context.Context) error {
					return context.DeadlineExceeded
				}
			case "upload", "tombstone":
				store.afterPut = func(context.Context) error { return errors.New("upload response lost") }
				if test.failure == "tombstone" {
					installProjectIconFailure(t, f, "tombstone")
				}
			case "revision":
				store.afterPut = func(context.Context) error {
					response := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", token, map[string]any{"icon": projecticon.Icon{Kind: "automatic"}, "expected_revision": "1", "idempotency_key": "concurrent-icon"})
					if response.Code != http.StatusOK {
						return fmt.Errorf("concurrent icon update status=%d", response.Code)
					}
					return nil
				}
			case "replay":
				store.afterPut = func(context.Context) error {
					store.afterPut = nil
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", token, body), http.StatusOK)
					return nil
				}
			case "cancel":
				store.afterPut = func(context.Context) error {
					cancel()
					return nil
				}
			}
			baseline, err := readProjectIconBytes(t.Context(), f.service, f.project.OrganizationID)
			if err != nil {
				t.Fatal(err)
			}
			store.beforePut = func(organization, id string, content []byte) {
				stored, err := readProjectIconAttachment(t.Context(), f.service, tracker.OrganizationID(organization), f.project.ID, id)
				if err != nil || stored.Metadata.DeletedAt != nil || stored.ObjectDeletedAt != nil {
					t.Fatalf("upload started without committed accounting: %v", err)
				}
				size, project := stored.Metadata.Size, stored.Metadata.ProjectID
				used, err := readProjectIconBytes(t.Context(), f.service, tracker.OrganizationID(organization))
				if err != nil {
					t.Fatal(err)
				}
				if size != int64(len(content)) || project != string(f.project.ID) || used < baseline+size {
					t.Fatalf("upload accounting size=%d content=%d project=%s usage=%d baseline=%d", size, len(content), project, used, baseline)
				}
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPut, f.base+"/icon", token, body).WithContext(ctx))
			requireNativeStatus(t, response, test.status)
			current, err := readProjectIconIntegration(t.Context(), f.service, hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID})
			if err != nil {
				t.Fatal(err)
			}
			wantObjects := 0
			if test.icon.Kind == "image" && test.failure != "quota" && test.icon.ImageKey == "" {
				wantObjects = 1
			}
			if test.failure == "replay" {
				wantObjects = 2
			}
			if test.status == http.StatusOK {
				if current.Icon == nil || len(store.objects) != wantObjects || len(store.objects[string(f.project.OrganizationID)+"/"+current.Icon.ImageKey]) == 0 {
					t.Fatalf("committed icon missing: %+v, objects=%v", current.Icon, store.objects)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/icon", token, body), http.StatusOK)
				if len(store.objects) != wantObjects {
					t.Fatal("replay uploaded another object")
				}
			} else {
				wantRevision := tracker.Revision(1)
				if test.failure == "revision" {
					wantRevision++
				}
				if current.Icon != nil || current.Revision != wantRevision || len(store.objects) != wantObjects {
					t.Fatalf("refused update changed project or retained objects: %+v, objects=%v", current, store.objects)
				}
			}
			var count int
			if err := usagePostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID).Scan(&count); err != nil || count != wantObjects {
				t.Fatalf("accounted rows=%d want=%d err=%v", count, wantObjects, err)
			}
			sweep := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/internal/v1/attachments/expired", nil), sweep)
			c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: cloudassert.KindService, Audience: string(f.project.OrganizationID)})
			if err := f.service.expiredCloudAttachments(c); err != nil {
				t.Fatal(err)
			}
			var expired []attachment.Metadata
			if err := json.Unmarshal(sweep.Body.Bytes(), &expired); err != nil {
				t.Fatal(err)
			}
			for key, content := range store.objects {
				id := strings.TrimPrefix(key, string(f.project.OrganizationID)+"/")
				stored, err := readProjectIconAttachment(t.Context(), f.service, f.project.OrganizationID, f.project.ID, id)
				if err != nil {
					t.Fatal(err)
				}
				deleted, counted := stored.Metadata.DeletedAt != nil, stored.ObjectDeletedAt == nil
				wantDeleted := (current.Icon == nil || current.Icon.ImageKey != id) && test.failure != "tombstone"
				found := false
				for _, item := range expired {
					if item.ID == id {
						found = true
					}
				}
				used, err := readProjectIconBytes(t.Context(), f.service, f.project.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				if deleted != wantDeleted || found != wantDeleted || !counted || used < baseline+int64(len(content)) {
					t.Fatalf("icon deleted=%t swept=%t want=%t counted=%t usage=%d", deleted, found, wantDeleted, counted, used)
				}
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
	saved, err := readProjectIconIntegration(t.Context(), f.service, hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID})
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
		{"owner", "owner", "", 200}, {"member", "owner", "", 403}, {"viewer", "viewer", "", 403}, {"stale", "owner", "999", 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBrowserHostedFixture(t, true)
			scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project)}
			before, err := readProjectIconIntegration(t.Context(), f.service, scope)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "member" {
				if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_owner", "member"); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), "user_browser_owner")
					if err != nil {
						return err
					}
					member.Role, member.UpdatedAt = "member", f.service.config.now().UTC()
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
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
				bootstrap := f.appRequest(t, "owner", http.MethodGet, "/app/bootstrap")
				requireNativeStatus(t, bootstrap, http.StatusOK)
				var payload appBootstrap
				decodeHubResponse(t, bootstrap, &payload)
				projects := payload.Projects
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
				after, err := readProjectIconIntegration(t.Context(), f.service, scope)
				if err != nil || after.Icon != nil || after.Revision != before.Revision {
					t.Fatalf("refusal changed project: %+v %v", after, err)
				}
			}
		})
	}
}

func readProjectIconIntegration(ctx context.Context, service *Service, scope hubscope.NativeScope) (ProjectIntegration, error) {
	record, err := service.readProjectRecord(ctx, scope)
	if err != nil {
		return ProjectIntegration{}, err
	}
	return projectIntegrationRecord(record), nil
}

func readProjectIconAttachment(ctx context.Context, service *Service, organization tracker.OrganizationID, project tracker.ProjectID, id string) (attachmentstore.Record, error) {
	var record attachmentstore.Record
	err := service.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.Attachment(ctx, project, id)
		return err
	})
	return record, err
}

func readProjectIconBytes(ctx context.Context, service *Service, organization tracker.OrganizationID) (int64, error) {
	var used int64
	err := service.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		used, err = r.CollaborationBytes(ctx)
		return err
	})
	return used, err
}

func installProjectIconFailure(t *testing.T, f nativeFixture, failure string) {
	t.Helper()
	var table, column, message string
	switch failure {
	case "mutation":
		table, column, message = "project_settings", "icon_kind", "mutation unavailable"
	case "tombstone":
		table, column, message = "attachments", "deleted_at", "tombstone unavailable"
	default:
		t.Fatalf("unknown icon failure %q", failure)
	}
	name := "refuse_icon_" + failure
	organization := strings.ReplaceAll(string(f.project.OrganizationID), "'", "''")
	project := strings.ReplaceAll(string(f.project.ID), "'", "''")
	for _, statement := range []string{
		fmt.Sprintf("CREATE FUNCTION public.%s() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,pg_temp AS $$ BEGIN RAISE EXCEPTION '%s' USING ERRCODE='P0001'; END $$", name, message),
		"GRANT EXECUTE ON FUNCTION public." + name + "() TO " + pgx.Identifier{f.service.config.Postgres.Connection.User}.Sanitize(),
		fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE OF %s ON public.%s FOR EACH ROW WHEN (OLD.organization_id='%s' AND OLD.project_id='%s') EXECUTE FUNCTION public.%s()", name, column, table, organization, project, name),
	} {
		if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}
