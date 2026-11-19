package orgsettings_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type contentSettingsRequest struct {
	tracker.Mutation
	projectsettings.ContentChange
}

func TestProjectContentSettings(t *testing.T) {
	t.Parallel()
	first := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	second := hubfixture.NewNative(t, first.Hub, first.Project.OrganizationID, "other-project")
	read := func(t *testing.T, f hubfixture.Native) projectsettings.Content {
		t.Helper()
		response := f.Hub.Request(t, http.MethodGet, f.Base+"/content-settings", f.Token, nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var value projectsettings.Content
		hubfixture.Decode(t, response, &value)
		return value
	}
	t.Run("updates without workflow or runner", func(t *testing.T) {
		first.ClearPolicy(t)
		second.ClearPolicy(t)
		for _, f := range []hubfixture.Native{first, second} {
			if got := read(t, f); got.RequireVisualAttachments || got.Revision != 0 {
				t.Fatalf("default=%+v", got)
			}
		}
		worker := first.Worker(t, "content-settings-worker")
		for _, test := range []struct {
			name, token string
			status      int
		}{
			{"unauthenticated", "", http.StatusUnauthorized}, {"worker", worker, http.StatusForbidden}, {"other project", second.Token, http.StatusNotFound},
		} {
			request := contentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}
			hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPut, first.Base+"/content-settings", test.token, request), test.status)
		}
		for _, change := range []struct {
			fixture hubfixture.Native
			enable  bool
			key     string
		}{
			{first, true, "enable-first"}, {second, true, "enable-second"}, {second, false, "disable-second"},
		} {
			before := read(t, change.fixture)
			request := contentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: change.key}, ContentChange: projectsettings.ContentChange{ExpectedRevision: before.Revision, RequireVisualAttachments: change.enable}}
			response := first.Hub.Request(t, http.MethodPut, change.fixture.Base+"/content-settings", change.fixture.Token, request)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var saved projectsettings.Content
			hubfixture.Decode(t, response, &saved)
			if saved.RequireVisualAttachments != change.enable || saved.Revision != before.Revision+1 {
				t.Fatalf("saved=%+v", saved)
			}
			if change.key == "enable-first" {
				replay := first.Hub.Request(t, http.MethodPut, change.fixture.Base+"/content-settings", change.fixture.Token, request)
				hubfixture.RequireStatus(t, replay, http.StatusOK)
				if replay.Body.String() != response.Body.String() {
					t.Fatal("replay changed receipt")
				}
				request.RequireVisualAttachments = !change.enable
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPut, change.fixture.Base+"/content-settings", change.fixture.Token, request), http.StatusConflict)
				if got := read(t, change.fixture); got != saved {
					t.Fatalf("changed replay modified settings: %+v", got)
				}
				request.RequireVisualAttachments = change.enable
				request.IdempotencyKey += "-stale"
				hubfixture.RequireStatus(t, first.Hub.Request(t, http.MethodPut, change.fixture.Base+"/content-settings", change.fixture.Token, request), http.StatusConflict)
			}
		}
	})
	t.Run("restart retains project isolation", func(t *testing.T) {
		if err := first.Hub.Service.Close(); err != nil {
			t.Fatal(err)
		}
		hub := first.Hub.Reopen(t)
		first.Hub, second.Hub = hub, hub
		if got := read(t, first); !got.RequireVisualAttachments || got.Revision != 1 {
			t.Fatalf("first=%+v", got)
		}
		if got := read(t, second); got.RequireVisualAttachments || got.Revision != 2 {
			t.Fatalf("second=%+v", got)
		}
		for _, test := range []struct {
			f    hubfixture.Native
			want int
		}{{first, 422}, {second, 200}} {
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "visual"}, Title: "Visual source", Body: "https://example.test/screen.png" + hubfixture.IssueContractSections, State: "Todo"}
			hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, test.f.Base+"/work-items", test.f.Token, request), test.want)
		}
	})
}
