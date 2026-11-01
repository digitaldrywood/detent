package hubserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectContentSettings(t *testing.T) {
	t.Parallel()
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	var first, second nativeFixture
	read := func(t *testing.T, f nativeFixture) projectsettings.Content {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/content-settings", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var value projectsettings.Content
		decodeHubResponse(t, response, &value)
		return value
	}
	t.Run("updates without workflow or runner", func(t *testing.T) {
		first = newDefaultNativeFixture(t, cfg)
		second = newNativeFixture(t, first.service, first.project.OrganizationID, "other-project")
		clearNativeFixturePolicy(t, first)
		clearNativeFixturePolicy(t, second)
		for _, f := range []nativeFixture{first, second} {
			if got := read(t, f); got.RequireVisualAttachments || got.Revision != 0 {
				t.Fatalf("default=%+v", got)
			}
		}
		worker := first.worker(t, "content-settings-worker")
		for _, test := range []struct {
			name, token string
			status      int
		}{
			{"unauthenticated", "", http.StatusUnauthorized}, {"worker", worker, http.StatusForbidden}, {"other project", second.token, http.StatusNotFound},
		} {
			request := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, first.base+"/content-settings", test.token, request), test.status)
		}
		for _, change := range []struct {
			fixture nativeFixture
			enable  bool
			key     string
		}{
			{first, true, "enable-first"}, {second, true, "enable-second"}, {second, false, "disable-second"},
		} {
			before := read(t, change.fixture)
			request := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: change.key}, ContentChange: projectsettings.ContentChange{ExpectedRevision: before.Revision, RequireVisualAttachments: change.enable}}
			response := performHubAPIRequest(t, first.service, http.MethodPut, change.fixture.base+"/content-settings", change.fixture.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var saved projectsettings.Content
			decodeHubResponse(t, response, &saved)
			if saved.RequireVisualAttachments != change.enable || saved.Revision != before.Revision+1 {
				t.Fatalf("saved=%+v", saved)
			}
			replay := performHubAPIRequest(t, first.service, http.MethodPut, change.fixture.base+"/content-settings", change.fixture.token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("replay changed receipt")
			}
			request.RequireVisualAttachments = !change.enable
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, change.fixture.base+"/content-settings", change.fixture.token, request), http.StatusConflict)
			if got := read(t, change.fixture); got != saved {
				t.Fatalf("changed replay modified settings: %+v", got)
			}
			request.RequireVisualAttachments = change.enable
			request.IdempotencyKey += "-stale"
			requireNativeStatus(t, performHubAPIRequest(t, first.service, http.MethodPut, change.fixture.base+"/content-settings", change.fixture.token, request), http.StatusConflict)
		}
	})
	t.Run("restart retains project isolation", func(t *testing.T) {
		service := openTestService(t, cfg)
		first.service, second.service = service, service
		if got := read(t, first); !got.RequireVisualAttachments || got.Revision != 1 {
			t.Fatalf("first=%+v", got)
		}
		if got := read(t, second); got.RequireVisualAttachments || got.Revision != 2 {
			t.Fatalf("second=%+v", got)
		}
		for _, test := range []struct {
			f    nativeFixture
			want int
		}{{first, 422}, {second, 200}} {
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "visual"}, Title: "Visual source", Body: "https://example.test/screen.png" + issueContractTestSections, State: "Todo"}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, test.f.base+"/work-items", test.f.token, request), test.want)
		}
	})
}

func TestCloudContentSettingsAdministration(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	var revision tracker.Revision
	for _, role := range []string{"viewer", "member", "reporter", "owner", "admin"} {
		t.Run(role, func(t *testing.T) {
			user := f.user(t, role, role, role+"@example.test", "write", "")
			request := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: role}, ContentChange: projectsettings.ContentChange{ExpectedRevision: revision, RequireVisualAttachments: true}}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			response := f.serve(t, hostedSharedRequest{user: &user, method: http.MethodPut, target: f.base + "/content-settings", body: string(raw), csrf: cloudassert.CSRFToken("shared-"+user.identity.Subject, "org_security")})
			if role == "owner" || role == "admin" {
				requireNativeStatus(t, response, 200)
				revision++
			} else if response.Code != 401 && response.Code != 403 && response.Code != 404 {
				t.Fatalf("non-administrator status=%d %s", response.Code, response.Body.String())
			}
		})
	}
}
