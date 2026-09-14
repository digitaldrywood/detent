package hubserver

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestAppShellServing covers decisions section 12 "Serving": the React
// application shell answers every non-API GET, an unauthenticated request to
// anything but /login redirects to /login, and API paths stay JSON.
func TestAppShellServing(t *testing.T) {
	f := newConversationHostedFixture(t)
	for _, test := range []struct {
		name     string
		account  string
		path     string
		status   int
		location string
	}{
		{name: "anonymous root", path: "/", status: http.StatusSeeOther, location: "/login"},
		{name: "anonymous work", path: "/work/board", status: http.StatusSeeOther, location: "/login"},
		{name: "anonymous chat", path: "/chat/c/conv_1", status: http.StatusSeeOther, location: "/login"},
		{name: "anonymous login shell", path: "/login", status: http.StatusOK},
		{name: "root", account: "owner", path: "/", status: http.StatusOK},
		{name: "work", account: "owner", path: "/work", status: http.StatusOK},
		{name: "work item", account: "owner", path: "/work/items/item_1", status: http.StatusOK},
		{name: "organization", account: "owner", path: "/organization/members", status: http.StatusOK},
		{name: "projects", account: "owner", path: "/projects/prj_unknown", status: http.StatusOK},
		{name: "settings", account: "owner", path: "/settings/integration", status: http.StatusOK},
		{name: "fleet", account: "owner", path: "/fleet", status: http.StatusOK},
		{name: "support", account: "owner", path: "/support", status: http.StatusOK},
		{name: "chat", account: "owner", path: "/chat", status: http.StatusOK},
		{name: "login while signed in", account: "owner", path: "/login", status: http.StatusOK},
		{name: "unknown api", account: "owner", path: "/api/v2/unknown", status: http.StatusNotFound},
		{name: "unknown v1 api", account: "owner", path: "/api/v1/unknown", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.request(t, test.account, http.MethodGet, test.path, nil)
			browserHostedStatus(t, response, test.status)
			if test.location != "" && response.Header().Get("Location") != test.location {
				t.Fatalf("location = %q, want %q", response.Header().Get("Location"), test.location)
			}
			if test.status != http.StatusOK {
				return
			}
			if !strings.Contains(response.Body.String(), `id="root"`) {
				t.Fatalf("shell body = %s", response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
				t.Fatalf("content type = %q", got)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("cache control = %q", got)
			}
		})
	}
	t.Run("missing client bundle", func(t *testing.T) {
		restore := conversationClientFS
		conversationClientFS = fstest.MapFS{}
		t.Cleanup(func() { conversationClientFS = restore })
		response := f.request(t, "owner", http.MethodGet, "/work", nil)
		browserHostedStatus(t, response, http.StatusServiceUnavailable)
		var failure apiErrorResponse
		decodeHubResponse(t, response, &failure)
		if failure.Code != "client_unavailable" {
			t.Fatalf("code = %q", failure.Code)
		}
	})
	t.Run("static bundle", func(t *testing.T) {
		browserHostedStatus(t, f.request(t, "", http.MethodGet, "/static/app/conversation/app.js", nil), http.StatusOK)
	})
}

// TestAppLogoutAnswersJSONCallers covers the section 12 logout rule: a JSON
// caller receives 204 and a form caller keeps the redirect.
func TestAppLogoutAnswersJSONCallers(t *testing.T) {
	f := newConversationHostedFixture(t)
	response := f.request(t, "owner", http.MethodPost, "/logout", nil)
	browserHostedStatus(t, response, http.StatusNoContent)
	response = f.form(t, "member", "/logout", nil)
	browserHostedStatus(t, response, http.StatusSeeOther)
	if response.Header().Get("Location") != "/login" {
		t.Fatalf("location = %q", response.Header().Get("Location"))
	}
}

// TestAppBootstrapPayload covers the section 12 bootstrap payload on both the
// new path and the retained /chat/bootstrap alias.
func TestAppBootstrapPayload(t *testing.T) {
	f := newConversationHostedFixture(t)
	browserHostedStatus(t, f.request(t, "", http.MethodGet, "/app/bootstrap", nil), http.StatusUnauthorized)
	for _, path := range []string{"/app/bootstrap", "/chat/bootstrap"} {
		t.Run(path, func(t *testing.T) {
			response := f.request(t, "owner", http.MethodGet, path, nil)
			browserHostedStatus(t, response, http.StatusOK)
			var payload appBootstrap
			decodeHubResponse(t, response, &payload)
			if payload.Organization.ID != conversationHostedOrganization || payload.Organization.Name != "Chat organization" {
				t.Fatalf("organization = %#v", payload.Organization)
			}
			if len(payload.Organizations) != 1 || payload.Organizations[0].ID != conversationHostedOrganization || !payload.Organizations[0].Current || payload.Organizations[0].PublicURL == "" {
				t.Fatalf("organizations = %#v", payload.Organizations)
			}
			if payload.Actor.Subject != "user_chat_owner" || payload.Actor.Role != "owner" || !payload.Actor.CanManage || payload.Actor.CanManageRunners {
				t.Fatalf("actor = %#v", payload.Actor)
			}
			if len(payload.Projects) != 1 || payload.Projects[0].ID != f.project || payload.Projects[0].Profile != "native" || !payload.Projects[0].CanWrite || len(payload.Projects[0].States) != 3 {
				t.Fatalf("projects = %#v", payload.Projects)
			}
			if payload.Support != nil {
				t.Fatalf("support = %#v", payload.Support)
			}
			if payload.Plan == nil || payload.Plan.ID == "" || payload.Plan.Source != "base" || payload.Plan.WindowEndsAt == "" {
				t.Fatalf("plan = %#v", payload.Plan)
			}
			if payload.APIBase != "/api/v2/organizations/"+conversationHostedOrganization || !payload.Feature.Conversation {
				t.Fatalf("bootstrap = %#v", payload)
			}
			if payload.CSRFToken != hostedCSRF(f.cookies["owner"].Value) {
				t.Fatalf("csrf = %q", payload.CSRFToken)
			}
		})
	}
	t.Run("viewer cannot write", func(t *testing.T) {
		response := f.request(t, "viewer", http.MethodGet, "/app/bootstrap", nil)
		browserHostedStatus(t, response, http.StatusOK)
		var payload appBootstrap
		decodeHubResponse(t, response, &payload)
		if payload.Actor.CanManage || len(payload.Projects) != 1 || payload.Projects[0].CanWrite {
			t.Fatalf("viewer bootstrap = %#v", payload)
		}
	})
}

// TestAppBootstrapPreferences covers the turn preference choices decisions
// section 14 adds: every list leads with "auto", the model list is what the
// enrolled runners report, and a hub whose runners report nothing still
// answers with "auto" alone.
func TestAppBootstrapPreferences(t *testing.T) {
	f := newConversationHostedFixture(t)
	payload := appBootstrapPayloadFor(t, f)
	for name, choices := range map[string][]appBootstrapChoice{
		"models": payload.Preferences.Models, "efforts": payload.Preferences.Efforts, "access": payload.Preferences.Access,
	} {
		if len(choices) == 0 || choices[0].ID != conversation.PreferenceAuto || choices[0].Label != "Auto" {
			t.Fatalf("%s = %#v, want Auto first", name, choices)
		}
	}
	if len(payload.Preferences.Models) != 1 {
		t.Fatalf("models = %#v, want only Auto before a runner reports one", payload.Preferences.Models)
	}
	if !payload.Preferences.Models[0].Default {
		t.Fatal("Auto must carry the default flag when no project default is known")
	}
	if got := conversationChoiceIDs(payload.Preferences.Efforts); strings.Join(got, ",") != "auto,low,medium,high" {
		t.Fatalf("efforts = %v", got)
	}
	if got := conversationChoiceIDs(payload.Preferences.Access); strings.Join(got, ",") != "auto,read_only,full" {
		t.Fatalf("access = %v", got)
	}

	// Once a runner reports a catalog, its models become the choices.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner = 1 WHERE user_id = ?", "user_chat_owner"); err != nil {
		t.Fatal(err)
	}
	hostedRunner(t, f, []providercapacity.Report{capacityReport(f.service.config.now())})
	payload = appBootstrapPayloadFor(t, f)
	if got := conversationChoiceIDs(payload.Preferences.Models); strings.Join(got, ",") != "auto,test-model" {
		t.Fatalf("models = %v", got)
	}
	if payload.Preferences.Models[1].Label != "test-model" {
		t.Fatalf("model label = %q", payload.Preferences.Models[1].Label)
	}
	// A report that carries only identifiers publishes only identifiers: the
	// picker then has no ladder to show and falls back to the fixed one.
	if model := payload.Preferences.Models[1]; len(model.Efforts) != 0 || model.DefaultEffort != "" || model.Legacy {
		t.Fatalf("model = %#v, want no invented detail", model)
	}
	if payload.Preferences.Models[1].Provider != "openai" {
		t.Fatalf("model provider = %q, want the reporting backend's", payload.Preferences.Models[1].Provider)
	}
}

// TestAppBootstrapPublishesModelDetail proves the per-model detail a runner's
// backend catalog reported reaches the composer's pickers: the model's own
// reasoning ladder, the rung it defaults to, its provider and whether the
// provider has named a successor (decisions section 14).
func TestAppBootstrapPublishesModelDetail(t *testing.T) {
	f := newConversationHostedFixture(t)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner = 1 WHERE user_id = ?", "user_chat_owner"); err != nil {
		t.Fatal(err)
	}
	report := capacityReport(f.service.config.now())
	report.Models = []string{"test-model", "retired-model"}
	report.ModelDetails = []providercapacity.ModelDetail{
		{
			ID: "test-model", Label: "Test Model", Provider: "openai", Default: true,
			ReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, DefaultReasoningEffort: "medium",
		},
		{ID: "retired-model", Label: "Retired Model", Legacy: true, ReasoningEfforts: []string{"low"}},
	}
	hostedRunner(t, f, []providercapacity.Report{report})

	payload := appBootstrapPayloadFor(t, f)
	// The runner's own order, not the alphabet: a provider lists its
	// catalogue in the order it wants read, and sorting put "retired-model"
	// above the backend's own default. "auto" still leads.
	if got := conversationChoiceIDs(payload.Preferences.Models); strings.Join(got, ",") != "auto,test-model,retired-model" {
		t.Fatalf("models = %v", got)
	}
	for _, test := range []struct {
		name, id, label, provider, defaultEffort string
		efforts                                  []string
		legacy, backendDefault                   bool
	}{
		{
			name: "a described model carries its own ladder", id: "test-model", label: "Test Model",
			provider: "openai", efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "medium",
			backendDefault: true,
		},
		{
			name: "a superseded model is published as legacy", id: "retired-model", label: "Retired Model",
			provider: "openai", efforts: []string{"low"}, legacy: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := slices.IndexFunc(payload.Preferences.Models, func(choice appBootstrapChoice) bool { return choice.ID == test.id })
			if index < 0 {
				t.Fatalf("models = %#v, want %q", payload.Preferences.Models, test.id)
			}
			choice := payload.Preferences.Models[index]
			if choice.Label != test.label || choice.Provider != test.provider || choice.Legacy != test.legacy {
				t.Fatalf("choice = %#v", choice)
			}
			if choice.DefaultEffort != test.defaultEffort || !slices.Equal(choice.Efforts, test.efforts) {
				t.Fatalf("choice = %#v, want efforts %v defaulting to %q", choice, test.efforts, test.defaultEffort)
			}
			// The backend's default is a different fact from the hub's: this
			// hub configures no coordinator model, so "auto" holds Default
			// while the catalogue's own pick holds BackendDefault.
			if choice.BackendDefault != test.backendDefault || choice.Default {
				t.Fatalf("choice = %#v, want backend default %t and no hub default", choice, test.backendDefault)
			}
		})
	}

	// An effort from a model's own ladder is accepted for that model even
	// though the fixed vocabulary has no such rung: the hub must not offer a
	// choice it would then refuse.
	created := f.request(t, "owner", http.MethodPost, f.base+"/conversations",
		map[string]any{"key": "conv-detail-1", "title": "Ladder"})
	browserHostedStatus(t, created, http.StatusCreated)
	var record conversationCreateResponse
	decodeHubResponse(t, created, &record)
	updated := f.request(t, "owner", http.MethodPatch, f.base+"/conversations/"+record.Conversation.ID,
		map[string]any{"preferences": map[string]any{"model": "test-model", "reasoning_effort": "xhigh"}})
	browserHostedStatus(t, updated, http.StatusOK)
	refused := f.request(t, "owner", http.MethodPatch, f.base+"/conversations/"+record.Conversation.ID,
		map[string]any{"preferences": map[string]any{"model": "retired-model", "reasoning_effort": "xhigh"}})
	browserHostedStatus(t, refused, http.StatusUnprocessableEntity)
}

func appBootstrapPayloadFor(t *testing.T, f *conversationHostedFixture) appBootstrap {
	t.Helper()
	response := f.request(t, "owner", http.MethodGet, "/app/bootstrap", nil)
	browserHostedStatus(t, response, http.StatusOK)
	var payload appBootstrap
	decodeHubResponse(t, response, &payload)
	return payload
}

func conversationChoiceIDs(choices []appBootstrapChoice) []string {
	ids := make([]string, 0, len(choices))
	for _, choice := range choices {
		ids = append(ids, choice.ID)
	}
	return ids
}

// TestWorkItemListIncludes covers the section 12 list enrichment: the default
// response is unchanged and include=attempts,changes adds the running attempt
// and the changes each card needs, bounded to the page.
func TestWorkItemListIncludes(t *testing.T) {
	f := newBrowserHostedFixture(t, true)
	base := browserHostedOrganizationBase + "/projects/" + f.project
	var plain struct {
		Items []map[string]any `json:"items"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/work-items", nil, http.StatusOK), &plain)
	if len(plain.Items) == 0 {
		t.Fatalf("items = %#v", plain.Items)
	}
	for _, field := range []string{"latest_attempt", "changes"} {
		if _, present := plain.Items[0][field]; present {
			t.Fatalf("default list carried %q", field)
		}
	}
	// The change goes on an item that has never run, so the same item shows
	// both an included value and an included null.
	var probe struct {
		Items []struct {
			WorkItemID    string          `json:"work_item_id"`
			LatestAttempt json.RawMessage `json:"latest_attempt"`
		} `json:"items"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/work-items?include=attempts", nil, http.StatusOK), &probe)
	item := ""
	for _, candidate := range probe.Items {
		if string(candidate.LatestAttempt) == "null" {
			item = candidate.WorkItemID
			break
		}
	}
	if item == "" {
		t.Fatalf("every item has an attempt: %+v", probe.Items)
	}
	f.api(t, "owner", http.MethodPost, base+"/work-items/"+item+"/changes", map[string]any{
		"idempotency_key": "list-change", "title": "fix(checklock): renew waits", "body": "Blocks the renewal.",
	}, http.StatusOK)
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	var enriched struct {
		Items []struct {
			WorkItemID    string `json:"work_item_id"`
			LatestAttempt *struct {
				Status    string `json:"status"`
				StartedAt string `json:"started_at"`
			} `json:"latest_attempt"`
			Changes []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				State string `json:"state"`
				URL   string `json:"url"`
			} `json:"changes"`
		} `json:"items"`
	}
	response := f.api(t, "owner", http.MethodGet, base+"/work-items?include=attempts,changes", nil, http.StatusOK)
	browserHostedDecode(t, response, &enriched)
	browserHostedDecode(t, response, &raw)
	index := -1
	for i, candidate := range enriched.Items {
		if candidate.WorkItemID == item {
			index = i
		}
	}
	if len(enriched.Items) != len(plain.Items) || index < 0 {
		t.Fatalf("enriched items = %#v", enriched.Items)
	}
	// An included field is always present, even when it has no value.
	if string(raw.Items[index]["latest_attempt"]) != "null" {
		t.Fatalf("latest_attempt = %s, want an explicit null", raw.Items[index]["latest_attempt"])
	}
	for i, other := range raw.Items {
		if i != index && string(other["changes"]) != "[]" {
			t.Fatalf("changes = %s, want an empty array", other["changes"])
		}
	}
	if enriched.Items[index].LatestAttempt != nil {
		t.Fatalf("latest attempt = %#v without a run", enriched.Items[index].LatestAttempt)
	}
	if len(enriched.Items[index].Changes) != 1 || enriched.Items[index].Changes[0].Title != "fix(checklock): renew waits" || enriched.Items[index].Changes[0].State != "draft" {
		t.Fatalf("changes = %#v", enriched.Items[index].Changes)
	}
	for i, other := range enriched.Items {
		if i != index && len(other.Changes) != 0 {
			t.Fatalf("unlinked item carried changes: %#v", other.Changes)
		}
	}
	f.api(t, "owner", http.MethodGet, base+"/work-items?include=nonsense", nil, http.StatusUnprocessableEntity)
	f.api(t, "owner", http.MethodGet, base+"/work-items?include=attempts", nil, http.StatusOK)
	f.api(t, "owner", http.MethodGet, base+"/work-items?include=coordinator,changes", nil, http.StatusOK)
}

// TestProjectEventsMovedToNativeBase covers the section 12 move: the stream
// now lives under the native project base and the old path stays an alias.
func TestProjectEventsMovedToNativeBase(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	f.seedIssue(t, 1)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
	stranger := f.user(t, "stranger", "member", "stranger@example.test", "", "")
	server := httptest.NewServer(f.service.Handler())
	t.Cleanup(server.Close)
	for _, test := range []struct {
		name string
		path string
		user hostedSecurityUser
		want int
	}{
		{name: "native base", path: hostedSecurityOrganizationBase + "/projects/" + string(f.project) + "/events", user: user, want: http.StatusOK},
		{name: "legacy alias", path: "/projects/" + string(f.project) + "/events", user: user, want: http.StatusOK},
		{name: "ungranted", path: hostedSecurityOrganizationBase + "/projects/" + string(f.project) + "/events", user: stranger, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(&http.Cookie{Name: hostedCookie, Value: test.user.token})
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.want {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.want)
			}
			if test.want != http.StatusOK {
				return
			}
			scanner := bufio.NewScanner(response.Body)
			if !scanner.Scan() || scanner.Text() != "event: activity" {
				t.Fatalf("stream frame = %q, error = %v", scanner.Text(), scanner.Err())
			}
		})
	}
}

// TestConversationClientIdentity covers the hash behind the footer's update
// pill: the served bundle's own bytes identify the build, two different
// bundles never share one, and a hub with no client built into it reports no
// build rather than failing to start.
func TestConversationClientIdentity(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 12, 8, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	bundle := func(body string) fstest.MapFS {
		return fstest.MapFS{conversationClientEntry: &fstest.MapFile{Data: []byte(body)}}
	}
	for _, test := range []struct {
		name    string
		fsys    fs.FS
		version string
		want    appClientBuild
	}{
		{
			name:    "missing bundle",
			fsys:    fstest.MapFS{},
			version: "v1.2.3",
			want:    appClientBuild{Version: "v1.2.3", ServedAt: "2026-09-12T06:30:00Z"},
		},
		{
			name:    "version is trimmed",
			fsys:    fstest.MapFS{},
			version: "  v1.2.3\n",
			want:    appClientBuild{Version: "v1.2.3", ServedAt: "2026-09-12T06:30:00Z"},
		},
		{
			name:    "unversioned hub still identifies its bundle",
			fsys:    bundle("console.log(1)"),
			version: "",
			want:    appClientBuild{Build: sha256Hex("console.log(1)"), ServedAt: "2026-09-12T06:30:00Z"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := conversationClientIdentity(test.fsys, test.version, at); got != test.want {
				t.Fatalf("identity = %#v, want %#v", got, test.want)
			}
		})
	}
	t.Run("a changed bundle is a changed build", func(t *testing.T) {
		t.Parallel()
		first := conversationClientIdentity(bundle("console.log(1)"), "v1.2.3", at)
		second := conversationClientIdentity(bundle("console.log(2)"), "v1.2.3", at)
		if first.Build == "" || first.Build == second.Build {
			t.Fatalf("builds = %q and %q", first.Build, second.Build)
		}
		// The same bytes on a later start are the same build: only the moment
		// it began being served moves, so a restart alone never tells a reader
		// their client is stale.
		again := conversationClientIdentity(bundle("console.log(1)"), "v1.2.3", at.Add(time.Hour))
		if again.Build != first.Build || again.ServedAt == first.ServedAt {
			t.Fatalf("restart = %#v, first = %#v", again, first)
		}
	})
}

// TestRunnerBehind covers the comparison the update report is built on: it is
// buildinfo's drift rule, so a placeholder version on either side is "cannot
// tell" and never lights an update badge on somebody's fleet.
func TestRunnerBehind(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		current  string
		reported string
		want     bool
	}{
		{name: "same build", current: "v1.2.3", reported: "v1.2.3"},
		{name: "older build", current: "v1.2.4", reported: "v1.2.3", want: true},
		// A runner ahead of the hub is still not on the hub's version, and the
		// operator still has a difference to reconcile.
		{name: "newer build", current: "v1.2.3", reported: "v1.2.4", want: true},
		{name: "runner never reported", current: "v1.2.3", reported: ""},
		{name: "runner on a development build", current: "v1.2.3", reported: "dev"},
		{name: "hub on a development build", current: "dev", reported: "v1.2.3"},
		{name: "neither is versioned", current: "", reported: ""},
		{name: "whitespace is not a version", current: "v1.2.3", reported: "  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := runnerBehind(test.current, test.reported); got != test.want {
				t.Fatalf("runnerBehind(%q, %q) = %v, want %v", test.current, test.reported, got, test.want)
			}
		})
	}
}

// TestAppUpdates covers GET /app/updates, the report behind the sidebar
// footer's pill: it needs a session, it names the hub's own build as what a
// runner is measured against, it counts the runners that are not on it, and it
// carries the served client build as its second line.
func TestAppUpdates(t *testing.T) {
	f := newConversationHostedFixture(t)
	browserHostedStatus(t, f.request(t, "", http.MethodGet, "/app/updates", nil), http.StatusUnauthorized)

	read := func(t *testing.T, account string) appUpdates {
		t.Helper()
		response := f.request(t, account, http.MethodGet, "/app/updates", nil)
		browserHostedStatus(t, response, http.StatusOK)
		var payload appUpdates
		decodeHubResponse(t, response, &payload)
		return payload
	}

	payload := read(t, "owner")
	if payload.Source != "hub" {
		t.Fatalf("source = %q, want hub", payload.Source)
	}
	if payload.Current != strings.TrimSpace(f.service.config.Version) {
		t.Fatalf("current = %q, want %q", payload.Current, f.service.config.Version)
	}
	if payload.Client != f.service.clientBuild || payload.Client.Build == "" {
		t.Fatalf("client = %#v, want %#v", payload.Client, f.service.clientBuild)
	}
	if len(payload.Runners) != 0 || payload.BehindCount != 0 {
		t.Fatalf("an unenrolled organization reports %#v", payload)
	}

	// A hub that knows its own version, and four enrolled runners: one on it,
	// one a release behind, one behind and silent past the heartbeat window,
	// and one whose enrolment was revoked and is nobody's to upgrade.
	f.service.config.Version = "v1.2.4"
	// Enrolling needs the runner grant, which this fixture's owner does not
	// hold by default.
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_project_grants SET manage_runner = 1 WHERE user_id = ?", "user_chat_owner"); err != nil {
		t.Fatal(err)
	}
	behind := hostedUpdateRunner(t, f, "Athens", "v1.2.3")
	offline := hostedUpdateRunner(t, f, "Cairo", "v1.2.3")
	revoked := hostedUpdateRunner(t, f, "Delhi", "v1.0.0")
	up := hostedUpdateRunner(t, f, "Dublin", "v1.2.4")
	stale := f.service.config.now().Add(-2 * runnerauth.HeartbeatTimeout).UTC().Format(time.RFC3339Nano)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET last_heartbeat_at = ? WHERE id = (SELECT machine_id FROM runner_identities WHERE id = ?)", stale, offline); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", stale, offline); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = (SELECT token_id FROM runner_identities WHERE id = ?)", f.service.config.now().UTC().Format(time.RFC3339Nano), revoked); err != nil {
		t.Fatal(err)
	}

	payload = read(t, "owner")
	if payload.Current != "v1.2.4" || payload.BehindCount != 2 {
		t.Fatalf("report = %#v", payload)
	}
	got := map[string]appUpdateRunner{}
	for _, runner := range payload.Runners {
		got[runner.RunnerID] = runner
	}
	if len(got) != 3 {
		t.Fatalf("runners = %#v; the revoked enrolment is not one", payload.Runners)
	}
	for _, want := range []appUpdateRunner{
		{RunnerID: behind, DisplayName: "Athens", Version: "v1.2.3", Online: true, Behind: true},
		{RunnerID: offline, DisplayName: "Cairo", Version: "v1.2.3", Online: false, Behind: true},
		{RunnerID: up, DisplayName: "Dublin", Version: "v1.2.4", Online: true, Behind: false},
	} {
		if got[want.RunnerID] != want {
			t.Fatalf("runner %s = %#v, want %#v", want.DisplayName, got[want.RunnerID], want)
		}
	}
	// Display name orders the report, so the pill's list reads the way the
	// settings screen's does.
	if payload.Runners[0].DisplayName != "Athens" || payload.Runners[2].DisplayName != "Dublin" {
		t.Fatalf("order = %#v", payload.Runners)
	}
	// Every member reads it: a viewer is as entitled to know the fleet is
	// behind as an owner, and can do nothing about it either way.
	if viewer := read(t, "viewer"); viewer.BehindCount != payload.BehindCount {
		t.Fatalf("viewer = %#v, owner = %#v", viewer, payload)
	}
}

// hostedUpdateRunner enrols one runner through the real enrolment exchange and
// heartbeats it, reporting a Detent build. It answers the runner's id.
func hostedUpdateRunner(t *testing.T, f *conversationHostedFixture, name string, version string) string {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	enrollments := "/api/v2/organizations/" + conversationHostedOrganization + "/runner-enrollments"
	response := f.request(t, "owner", http.MethodPost, enrollments, runnerauth.EnrollmentRequest{
		Binding:    binding,
		ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)},
		Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events},
		TTLSeconds: 60,
	})
	browserHostedStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	redemption := runnerauth.Redemption{
		Binding: binding, Credential: credential, Hostname: strings.ToLower(name) + ".example.test",
		DisplayName: name, Capacity: 1, Version: version,
	}
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, enrollments+"/redeem", enrollment.Token, redemption), http.StatusCreated)
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(binding.MachineID)+"/heartbeat", credential,
		map[string]any{"display_name": name, "capacity": 1, "version": version}), http.StatusNoContent)
	return binding.RunnerID
}

// sha256Hex is the digest conversationClientIdentity reports for a body.
func sha256Hex(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}
