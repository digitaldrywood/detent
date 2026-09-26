package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const appShellTestBody = `<!doctype html><div id="root"></div>`

func useAppClientFS(t *testing.T, fsys fs.FS) {
	t.Helper()
	restore := conversationClientFS
	conversationClientFS = fsys
	t.Cleanup(func() { conversationClientFS = restore })
}

func appClientBundle() fstest.MapFS {
	return fstest.MapFS{
		conversationClientShell: &fstest.MapFile{Data: []byte(appShellTestBody)},
		conversationClientEntry: &fstest.MapFile{Data: []byte("console.log(1)")},
	}
}

func (f *browserHostedFixture) appRequest(t *testing.T, account, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, f.server.URL+path, nil)
	request.Header.Set("Origin", f.server.URL)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestAppShellServing(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newBrowserHostedFixture(t, true)
	for _, test := range []struct {
		name     string
		account  string
		method   string
		path     string
		status   int
		location string
		shell    bool
	}{
		{name: "anonymous work", path: "/work/board", status: http.StatusSeeOther, location: "/login"},
		{name: "anonymous chat", path: "/chat/c/conv_1", status: http.StatusSeeOther, location: "/login"},
		{name: "work", account: "owner", path: "/work", status: http.StatusOK, shell: true},
		{name: "work item", account: "owner", path: "/work/items/item_1", status: http.StatusOK, shell: true},
		{name: "organization members", account: "owner", path: "/organization/members", status: http.StatusOK, shell: true},
		{name: "settings", account: "owner", path: "/settings/integration", status: http.StatusOK, shell: true},
		{name: "fleet", account: "owner", path: "/fleet", status: http.StatusOK, shell: true},
		{name: "chat", account: "owner", path: "/chat", status: http.StatusOK, shell: true},
		{name: "viewer", account: "viewer", path: "/work", status: http.StatusOK, shell: true},
		{name: "head", account: "owner", method: http.MethodHead, path: "/work", status: http.StatusOK, shell: true},
		{name: "templ root keeps its page", account: "owner", path: "/", status: http.StatusOK},
		{name: "templ login keeps its page", path: "/login", status: http.StatusOK},
		{name: "unknown api", account: "owner", path: "/api/v2/unknown", status: http.StatusNotFound},
		{name: "unknown v1 api", account: "owner", path: "/api/v1/unknown", status: http.StatusNotFound},
		{name: "reserved auth", account: "owner", path: "/auth/unknown", status: http.StatusNotFound},
		{name: "reserved webhooks", account: "owner", path: "/webhooks/unknown", status: http.StatusNotFound},
		{name: "reserved health", account: "owner", path: "/health", status: http.StatusNotFound},
		{name: "reserved logout", account: "owner", path: "/logout", status: http.StatusNotFound},
		{name: "reserved api root", account: "owner", path: "/api", status: http.StatusNotFound},
		{name: "reserved app root", account: "owner", path: "/app", status: http.StatusNotFound},
		{name: "reserved app subtree", account: "owner", path: "/app/unknown", status: http.StatusNotFound},
		{name: "reserved auth root", account: "owner", path: "/auth", status: http.StatusNotFound},
		{name: "reserved webhooks root", account: "owner", path: "/webhooks", status: http.StatusNotFound},
		{name: "reserved static root", account: "owner", path: "/static", status: http.StatusNotFound},
		{name: "reserved metrics", account: "owner", path: "/metrics", status: http.StatusNotFound},
		{name: "mutation", account: "owner", method: http.MethodPost, path: "/work", status: http.StatusNotFound},
		{name: "root mutation", account: "owner", method: http.MethodPost, path: "/", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			method := test.method
			if method == "" {
				method = http.MethodGet
			}
			response := f.appRequest(t, test.account, method, test.path)
			browserHostedStatus(t, response, test.status)
			if got := response.Header().Get("Location"); got != test.location {
				t.Fatalf("location = %q, want %q", got, test.location)
			}
			if got := response.Body.String() == appShellTestBody; got != test.shell {
				t.Fatalf("shell served = %t, want %t: %s", got, test.shell, response.Body.String())
			}
			if !test.shell {
				return
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
		useAppClientFS(t, fstest.MapFS{})
		response := f.appRequest(t, "owner", http.MethodGet, "/work")
		browserHostedStatus(t, response, http.StatusServiceUnavailable)
		var failure apiErrorResponse
		decodeHubResponse(t, response, &failure)
		if failure.Code != "client_unavailable" {
			t.Fatalf("code = %q", failure.Code)
		}
	})
}

func TestAppReserved(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/api/v2/organizations", want: true},
		{path: "/auth/oidc/start", want: true},
		{path: "/webhooks/stripe", want: true},
		{path: "/static/app/conversation/app.js", want: true},
		{path: "/api", want: true},
		{path: "/app", want: true},
		{path: "/app/bootstrap", want: true},
		{path: "/auth", want: true},
		{path: "/webhooks", want: true},
		{path: "/static", want: true},
		{path: "/metrics", want: true},
		{path: "/health", want: true},
		{path: "/invite", want: true},
		{path: "/logout", want: true},
		{path: "/healthy"},
		{path: "/invites"},
		{path: "/apis"},
		{path: "/application"},
		{path: "/metrics/x"},
		{path: "/work"},
		{path: "/"},
	} {
		t.Run(test.path, func(t *testing.T) {
			t.Parallel()
			if got := appReserved(test.path); got != test.want {
				t.Fatalf("appReserved(%q) = %t, want %t", test.path, got, test.want)
			}
		})
	}
}

func TestAppBootstrapPayload(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	browserHostedStatus(t, f.appRequest(t, "", http.MethodGet, "/app/bootstrap"), http.StatusUnauthorized)
	browserHostedStatus(t, f.appRequest(t, "staff", http.MethodGet, "/app/bootstrap"), http.StatusForbidden)
	for _, path := range []string{"/app/bootstrap", "/chat/bootstrap"} {
		t.Run(path, func(t *testing.T) {
			response := f.appRequest(t, "owner", http.MethodGet, path)
			browserHostedStatus(t, response, http.StatusOK)
			var raw map[string]json.RawMessage
			decodeHubResponse(t, response, &raw)
			keys := make([]string, 0, len(raw))
			for key := range raw {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if want := "actor,api_base,capabilities,csrf_token,feature,organization,organizations,plan,preferences,projects,support"; strings.Join(keys, ",") != want {
				t.Fatalf("keys = %v, want %s", keys, want)
			}
			for field, want := range map[string]string{
				"capabilities": `{"coordinator":false,"attachments":false}`,
				"feature":      `{"conversation":false}`,
				"preferences":  `{"models":[],"efforts":[],"access":[]}`,
				"support":      `null`,
			} {
				if got := string(raw[field]); got != want {
					t.Fatalf("%s = %s, want %s", field, got, want)
				}
			}
			var payload appBootstrap
			decodeHubResponse(t, response, &payload)
			if payload.Organization.ID != "org_browser_preview" || payload.Organization.Name == "" || !payload.Organization.Current {
				t.Fatalf("organization = %#v", payload.Organization)
			}
			if len(payload.Organizations) != 1 || payload.Organizations[0].ID != "org_browser_preview" || !payload.Organizations[0].Current || payload.Organizations[0].Name != payload.Organization.Name || payload.Organizations[0].PublicURL == "" {
				t.Fatalf("organizations = %#v", payload.Organizations)
			}
			if payload.Actor.Subject != "user_browser_owner" || payload.Actor.Role != "owner" || payload.Actor.Email != "owner@example.test" || !payload.Actor.CanManage || payload.Actor.PrincipalID == "" {
				t.Fatalf("actor = %#v", payload.Actor)
			}
			if len(payload.Projects) != 2 || !payload.Projects[0].CanWrite || payload.Projects[0].Profile != "native" {
				t.Fatalf("projects = %#v", payload.Projects)
			}
			if payload.APIBase != "/api/v2/organizations/org_browser_preview" || payload.CSRFToken != hostedCSRF(f.cookies["owner"].Value) {
				t.Fatalf("bootstrap = %#v", payload)
			}
			if payload.Plan == nil || payload.Plan.ID == "" || payload.Plan.WindowEndsAt == "" {
				t.Fatalf("plan = %#v", payload.Plan)
			}
		})
	}
	t.Run("viewer cannot write", func(t *testing.T) {
		response := f.appRequest(t, "viewer", http.MethodGet, "/app/bootstrap")
		browserHostedStatus(t, response, http.StatusOK)
		var payload appBootstrap
		decodeHubResponse(t, response, &payload)
		if payload.Actor.CanManage || payload.Actor.CanManageRunners || len(payload.Projects) != 1 || payload.Projects[0].ID != f.project || payload.Projects[0].CanWrite {
			t.Fatalf("viewer bootstrap = %#v", payload)
		}
	})
	t.Run("runner grants", func(t *testing.T) {
		grantAppRunners(t, f)
		response := f.appRequest(t, "owner", http.MethodGet, "/app/bootstrap")
		browserHostedStatus(t, response, http.StatusOK)
		var payload appBootstrap
		decodeHubResponse(t, response, &payload)
		if !payload.Actor.CanManageRunners || !payload.Projects[0].CanManageRunners {
			t.Fatalf("owner bootstrap = %#v", payload)
		}
	})
}

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
		{name: "missing bundle", fsys: fstest.MapFS{}, version: "v1.2.3", want: appClientBuild{Version: "v1.2.3", ServedAt: "2026-09-12T06:30:00Z"}},
		{name: "version is trimmed", fsys: fstest.MapFS{}, version: "  v1.2.3\n", want: appClientBuild{Version: "v1.2.3", ServedAt: "2026-09-12T06:30:00Z"}},
		{name: "unversioned hub still identifies its bundle", fsys: bundle("console.log(1)"), want: appClientBuild{Build: sha256Hex("console.log(1)"), ServedAt: "2026-09-12T06:30:00Z"}},
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
		again := conversationClientIdentity(bundle("console.log(1)"), "v1.2.3", at.Add(time.Hour))
		if again.Build != first.Build || again.ServedAt == first.ServedAt {
			t.Fatalf("restart = %#v, first = %#v", again, first)
		}
	})
}

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
		{name: "older minor", current: "v1.10.0", reported: "v1.9.9", want: true},
		{name: "prerelease of the hub's build", current: "v1.2.4", reported: "v1.2.4-rc.1", want: true},
		{name: "newer build", current: "v1.2.3", reported: "v1.2.4"},
		{name: "unprefixed versions", current: "1.2.4", reported: "1.2.3", want: true},
		{name: "not a release version", current: "v1.2.3", reported: "abc123"},
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

func TestAppUpdates(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newBrowserHostedFixture(t, true)
	read := func(t *testing.T, account string) appUpdates {
		t.Helper()
		response := f.appRequest(t, account, http.MethodGet, "/app/updates")
		browserHostedStatus(t, response, http.StatusOK)
		var payload appUpdates
		decodeHubResponse(t, response, &payload)
		return payload
	}
	for _, test := range []struct {
		account string
		status  int
	}{
		{status: http.StatusUnauthorized},
		{account: "staff", status: http.StatusForbidden},
		{account: "wrong-organization", status: http.StatusForbidden},
		{account: "invitee", status: http.StatusForbidden},
		{account: "viewer", status: http.StatusNotFound},
		{account: "owner", status: http.StatusNotFound},
	} {
		t.Run("denied "+test.account, func(t *testing.T) {
			browserHostedStatus(t, f.appRequest(t, test.account, http.MethodGet, "/app/updates"), test.status)
		})
	}
	grantAppRunners(t, f)
	browserHostedStatus(t, f.appRequest(t, "viewer", http.MethodGet, "/app/updates"), http.StatusNotFound)
	payload := read(t, "owner")
	if payload.Source != "hub" || payload.Current != strings.TrimSpace(f.service.config.Version) {
		t.Fatalf("report = %#v", payload)
	}
	if payload.Client != f.service.clientBuild || payload.Client.Build != sha256Hex("console.log(1)") {
		t.Fatalf("client = %#v, want %#v", payload.Client, f.service.clientBuild)
	}
	if len(payload.Runners) != 0 || payload.BehindCount != 0 {
		t.Fatalf("an unenrolled organization reports %#v", payload)
	}

	f.service.config.Version = "v1.2.4"
	behind := enrollAppRunner(t, f, "Athens", "v1.2.3")
	offline := enrollAppRunner(t, f, "Cairo", "v1.2.3")
	revoked := enrollAppRunner(t, f, "Delhi", "v1.0.0")
	up := enrollAppRunner(t, f, "Dublin", "v1.2.4")
	ahead := enrollAppRunner(t, f, "Essen", "v1.3.0")
	stale := formatHubTime(f.service.config.now().Add(-2 * runnerauth.HeartbeatTimeout))
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at = ? WHERE id = ?", stale, offline.RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = (SELECT token_id FROM runner_identities WHERE id = ?)", formatHubTime(f.service.config.now()), revoked.RunnerID); err != nil {
		t.Fatal(err)
	}

	payload = read(t, "owner")
	if payload.Current != "v1.2.4" || payload.BehindCount != 2 || len(payload.Runners) != 4 {
		t.Fatalf("report = %#v", payload)
	}
	for index, want := range []appUpdateRunner{
		{RunnerID: behind.RunnerID, DisplayName: "Athens", Version: "v1.2.3", Online: true, Behind: true},
		{RunnerID: offline.RunnerID, DisplayName: "Cairo", Version: "v1.2.3", Behind: true},
		{RunnerID: up.RunnerID, DisplayName: "Dublin", Version: "v1.2.4", Online: true},
		{RunnerID: ahead.RunnerID, DisplayName: "Essen", Version: "v1.3.0", Online: true},
	} {
		if payload.Runners[index] != want {
			t.Fatalf("runner %d = %#v, want %#v", index, payload.Runners[index], want)
		}
	}

	heartbeat := "/api/v2/organizations/org_browser_preview/projects/" + f.project + "/machines/" + string(behind.MachineID) + "/heartbeat"
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, heartbeat, behind.Credential, map[string]any{"display_name": "Athens", "capacity": 1, "version": "v1.2.4"}), http.StatusNoContent)
	payload = read(t, "owner")
	if payload.BehindCount != 1 || payload.Runners[0].Version != "v1.2.4" || payload.Runners[0].Behind {
		t.Fatalf("after upgrade heartbeat = %#v", payload)
	}
}

type appTestRunner struct {
	runnerauth.Binding
	Credential string
}

func grantAppRunners(t *testing.T, f *browserHostedFixture) {
	t.Helper()
	for _, project := range []string{f.project, f.privateProject} {
		browserHostedStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	}
}

func enrollAppRunner(t *testing.T, f *browserHostedFixture, name string, version string) appTestRunner {
	t.Helper()
	organization := "/api/v2/organizations/org_browser_preview"
	binding := runnerauth.NewBinding()
	request := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 900}
	response := f.setupRequest(t, "owner", http.MethodPost, organization+"/runner-enrollments", request)
	browserHostedStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: strings.ToLower(name) + ".example.test", DisplayName: name, Capacity: 1, Version: version}
	browserHostedStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, organization+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	return appTestRunner{Binding: binding, Credential: credential}
}

func sha256Hex(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}
