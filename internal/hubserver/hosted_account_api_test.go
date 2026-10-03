package hubserver

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestHostedAccountAPI(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	csrf := func(account string) map[string]string {
		return map[string]string{"X-CSRF-Token": hostedCSRF(f.cookies[account].Value)}
	}
	invite := f.rawAPI(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/members/invitations", `{"email":"invitee@example.test","role":"viewer","idempotency_key":"account-invite"}`, csrf("owner"))
	browserHostedStatus(t, invite, http.StatusCreated)
	var invitation string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM hosted_invitations WHERE email = 'invitee@example.test'").Scan(&invitation); err != nil {
		t.Fatal(err)
	}
	accept := `{"token":"` + invitation + `","idempotency_key":"accept"}`
	for _, test := range []struct {
		name, account, method, path, body string
		headers                           map[string]string
		status                            int
		code                              string
		check                             func(t *testing.T, body []byte)
	}{
		{name: "anonymous plan", method: http.MethodGet, path: "/plan", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "viewer plan", account: "viewer", method: http.MethodGet, path: "/plan", status: http.StatusForbidden, code: "forbidden"},
		{name: "staff plan", account: "staff", method: http.MethodGet, path: "/plan", status: http.StatusForbidden, code: "forbidden"},
		{name: "owner plan", account: "owner", method: http.MethodGet, path: "/plan", status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var plan HostedEntitlement
			if err := json.Unmarshal(body, &plan); err != nil {
				t.Fatal(err)
			}
			if plan.OrganizationID != "org_browser_preview" || plan.EffectiveBase.ID == "" || plan.Usage == nil || plan.Allowances == nil || plan.WindowEndsAt.IsZero() {
				t.Fatalf("plan = %#v", plan)
			}
			if _, ok := plan.Usage["events_total"]; ok {
				t.Fatalf("plan usage carries the internal events total: %#v", plan.Usage)
			}
		}},
		{name: "switch without CSRF", account: "owner", method: http.MethodPost, path: "/switch", body: `{"organization":"org_browser_preview","idempotency_key":"s"}`, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "anonymous switch", method: http.MethodPost, path: "/switch", body: `{"organization":"org_browser_preview","idempotency_key":"s"}`, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "switch with unknown field", account: "owner", method: http.MethodPost, path: "/switch", body: `{"organization":"org_browser_preview","target":"x"}`, headers: csrf("owner"), status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "switch to an unknown organization", account: "viewer", method: http.MethodPost, path: "/switch", body: `{"organization":"org_unknown","idempotency_key":"s"}`, headers: csrf("viewer"), status: http.StatusForbidden, code: "forbidden"},
		{name: "support session cannot switch", account: "support-viewer", method: http.MethodPost, path: "/switch", body: `{"organization":"org_browser_preview","idempotency_key":"s"}`, headers: csrf("support-viewer"), status: http.StatusForbidden, code: "forbidden"},
		{name: "owner switches", account: "owner", method: http.MethodPost, path: "/switch", body: `{"organization":"org_browser_preview","idempotency_key":"s"}`, headers: csrf("owner"), status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var next hostedNextResponse
			if err := json.Unmarshal(body, &next); err != nil {
				t.Fatal(err)
			}
			if next.Next != f.server.URL+"/auth/oidc/start" {
				t.Fatalf("next = %q", next.Next)
			}
		}},
		{name: "accept without CSRF", account: "invitee", method: http.MethodPost, path: "/invitations/accept", body: accept, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "wrong account accepts", account: "viewer", method: http.MethodPost, path: "/invitations/accept", body: accept, headers: csrf("viewer"), status: http.StatusForbidden, code: "forbidden"},
		{name: "staff accepts", account: "staff", method: http.MethodPost, path: "/invitations/accept", body: accept, headers: csrf("staff"), status: http.StatusForbidden, code: "forbidden"},
		{name: "invitee accepts", account: "invitee", method: http.MethodPost, path: "/invitations/accept", body: accept, headers: csrf("invitee"), status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var next hostedNextResponse
			if err := json.Unmarshal(body, &next); err != nil {
				t.Fatal(err)
			}
			if next.Next != f.server.URL+"/auth/oidc/start" {
				t.Fatalf("next = %q", next.Next)
			}
		}},
		{name: "invitation replay by the same account", account: "invitee", method: http.MethodPost, path: "/invitations/accept", body: accept, headers: csrf("invitee"), status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var next hostedNextResponse
			if err := json.Unmarshal(body, &next); err != nil {
				t.Fatal(err)
			}
			if next.Next != f.server.URL+"/auth/oidc/start" {
				t.Fatalf("next = %q", next.Next)
			}
		}},
		{name: "invitation replay by another account", account: "viewer", method: http.MethodPost, path: "/invitations/accept", body: accept, headers: csrf("viewer"), status: http.StatusForbidden, code: "forbidden"},
		{name: "ordinary staff cannot start support", account: "staff", method: http.MethodPost, path: "/support/start", body: `{"idempotency_key":"support"}`, headers: csrf("staff"), status: http.StatusForbidden, code: "forbidden"},
		{name: "member cannot start support", account: "owner", method: http.MethodPost, path: "/support/start", body: `{"idempotency_key":"support"}`, headers: csrf("owner"), status: http.StatusForbidden, code: "forbidden"},
		{name: "support without CSRF", account: "support-staff", method: http.MethodPost, path: "/support/start", body: `{"idempotency_key":"support"}`, status: http.StatusForbidden, code: "invalid_csrf"},
		{name: "authorized support starts", account: "support-staff", method: http.MethodPost, path: "/support/start", body: `{"idempotency_key":"support"}`, headers: csrf("support-staff"), status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var support hostedSupportResponse
			if err := json.Unmarshal(body, &support); err != nil {
				t.Fatal(err)
			}
			remaining := time.Until(support.Support.ExpiresAt)
			if support.Support.Actor != "support@example.test" || remaining <= 0 || remaining > 10*time.Minute {
				t.Fatalf("support = %#v", support)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.rawAPI(t, test.account, test.method, browserHostedOrganizationBase+test.path, test.body, test.headers)
			browserHostedStatus(t, response, test.status)
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
			}
			if test.check != nil {
				test.check(t, response.Body.Bytes())
				return
			}
			var failure apiErrorResponse
			browserHostedDecode(t, response, &failure)
			if test.code != "" && failure.Code != test.code || failure.Message == "" {
				t.Fatalf("error = %#v, want code %q", failure, test.code)
			}
		})
	}
	t.Run("another organization is not found", func(t *testing.T) {
		for _, path := range []string{"/plan", "/switch", "/invitations/accept", "/support/start"} {
			method := http.MethodPost
			if path == "/plan" {
				method = http.MethodGet
			}
			response := f.rawAPI(t, "owner", method, "/api/v2/organizations/org_other"+path, `{"idempotency_key":"k"}`, csrf("owner"))
			browserHostedStatus(t, response, http.StatusNotFound)
		}
	})
}

func TestHostedSharedAccountAPI(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newHostedSharedFixture(t)
	owner := f.member(t, "owner", "owner", "write")
	viewer := f.member(t, "viewer", "viewer", "read")
	csrf := cloudassert.CSRFToken("shared-user_owner", "org_security")
	api := "/api/v2/organizations/org_security"
	for _, test := range []struct {
		name    string
		request hostedSharedRequest
		status  int
	}{
		{name: "owner plan", request: hostedSharedRequest{user: &owner, target: api + "/plan"}, status: http.StatusOK},
		{name: "viewer plan", request: hostedSharedRequest{user: &viewer, target: api + "/plan"}, status: http.StatusForbidden},
		{name: "switch belongs to the entry", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/switch", body: `{"organization":"org_security","idempotency_key":"s"}`, csrf: csrf}, status: http.StatusNotFound},
		{name: "accept belongs to the entry", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/invitations/accept", body: `{"token":"t","idempotency_key":"a"}`, csrf: csrf}, status: http.StatusNotFound},
		{name: "support belongs to the entry", request: hostedSharedRequest{user: &owner, method: http.MethodPost, target: api + "/support/start", body: `{"idempotency_key":"s"}`, csrf: csrf}, status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(t, test.request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestHostedAccountContractFixtures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		fixture  string
		target   func() any
		optional []string
	}{
		{fixture: "account-plan.json", target: func() any { return &HostedEntitlement{} }, optional: []string{"grants[].revoked_at"}},
		{fixture: "account-next.json", target: func() any { return &hostedNextResponse{} }},
		{fixture: "account-support.json", target: func() any { return &hostedSupportResponse{} }},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join(conversationFixtureDirectory, test.fixture))
			if err != nil {
				t.Skipf("shared contract fixture is not available: %v", err)
			}
			value := test.target()
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatalf("decode fixture into the Go payload: %v", err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			compareConversationShape(t, "", want, got, conversationPathSet(nil), conversationPathSet(test.optional))
		})
	}
}

var accountClientCall = regexp.MustCompile("send\\(\\s*[\\w.]+,\\s*\"([A-Z]+)\",\\s*(?:`([^`]+)`|hubPath\\(\"([^\"]+)\"\\))")

var accountClientParameter = regexp.MustCompile(`\$\{encodeURIComponent\([^}]*\)\}`)

// TestHostedAccountClientRoutesMounted reads every call the client's account
// API makes and requires a hosted Hub route for it, so a screen cannot ship
// against an endpoint the Hub never mounted.
func TestHostedAccountClientRoutesMounted(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../web/conversation/src/app/account/api.ts")
	if err != nil {
		t.Skipf("account client is not available: %v", err)
	}
	f := newBrowserHostedFixture(t, true)
	mounted := make(map[string]bool)
	for _, route := range f.service.echo.Routes() {
		mounted[route.Method+" "+route.Path] = true
	}
	// Cloud entry owns provisioning and attachment objects. Its attachment route
	// isolation tests exercise both API path forms and tenant/project authorization.
	entryOwned := map[string]bool{
		"POST /api/v2/organizations":                                                       true,
		"POST " + hostedOrganizationBase + "/projects/:project/attachments":                true,
		"GET " + hostedOrganizationBase + "/projects/:project/attachments/:param/metadata": true,
	}
	calls := accountClientCall.FindAllStringSubmatch(string(raw), -1)
	if len(calls) < 20 {
		t.Fatalf("found %d account client calls; the call pattern no longer matches api.ts", len(calls))
	}
	for _, call := range calls {
		path := call[2]
		if call[3] != "" {
			path = call[3]
		}
		path = strings.ReplaceAll(path, "${project(projectId)}", "${base}/projects/:project")
		path = strings.ReplaceAll(path, "${project(input.projectId)}", "${base}/projects/:project")
		path = strings.ReplaceAll(path, "${base}", hostedOrganizationBase)
		path = accountClientParameter.ReplaceAllString(path, ":param")
		for _, key := range accountClientVariants(call[1] + " " + path) {
			if entryOwned[key] {
				continue
			}
			t.Run(key, func(t *testing.T) {
				if mounted[key] {
					return
				}
				for route := range mounted {
					if accountClientRouteMatches(route, key) {
						return
					}
				}
				t.Fatalf("the account client calls %s, which the hosted Hub does not mount", key)
			})
		}
	}
}

// Route parameters match any nonempty path segment, including literal client
// values such as fly_sprites_token. Static segments and methods stay exact.
func accountClientRouteMatches(route, call string) bool {
	pattern := regexp.MustCompile("^" + regexp.MustCompile(`:[^/]+`).ReplaceAllString(regexp.QuoteMeta(route), `[^/]+`) + "$")
	return pattern.MatchString(call)
}

func TestAccountClientRouteMatches(t *testing.T) {
	t.Parallel()
	const route = "GET " + nativeBase + "/secrets/:kind"
	const call = "GET " + nativeBase + "/secrets/fly_sprites_token"
	for _, test := range []struct {
		name, route, call string
		want              bool
	}{
		{name: "exact route", route: call, call: call, want: true},
		{name: "literal kind", route: route, call: call, want: true},
		{name: "parameter names differ", route: route, call: strings.Replace(call, ":project", ":param", 1), want: true},
		{name: "wrong method", route: route, call: strings.Replace(call, "GET ", "PUT ", 1)},
		{name: "wrong resource", route: route, call: strings.Replace(call, "/secrets/", "/missing/", 1)},
		{name: "missing segment", route: route, call: strings.TrimSuffix(call, "/fly_sprites_token")},
		{name: "empty segment", route: route, call: strings.TrimSuffix(call, "fly_sprites_token")},
		{name: "extra segment", route: route, call: call + "/extra"},
		{name: "extra prefix", route: route, call: "prefix " + call},
		{name: "static regexp characters", route: "GET /api/v2/plan.json", call: "GET /api/v2/planXjson"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := accountClientRouteMatches(test.route, test.call); got != test.want {
				t.Fatalf("accountClientRouteMatches(%q, %q) = %t, want %t", test.route, test.call, got, test.want)
			}
		})
	}
}

var accountClientOptionalSegment = regexp.MustCompile(`\$\{[^}]*\? "([^"]*)" : ""\}`)

// accountClientVariants expands a conditional path segment into the path with
// and without it.
func accountClientVariants(key string) []string {
	match := accountClientOptionalSegment.FindStringSubmatch(key)
	if match == nil {
		return []string{key}
	}
	return []string{strings.Replace(key, match[0], match[1], 1), strings.Replace(key, match[0], "", 1)}
}

func TestAppBootstrapRefusalCarriesSupportCSRF(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for _, test := range []struct {
		name, account string
		status        int
		csrf          bool
	}{
		{name: "support actor without access", account: "support-staff", status: http.StatusForbidden, csrf: true},
		{name: "ordinary staff", account: "staff", status: http.StatusForbidden, csrf: true},
		{name: "anonymous", status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.rawAPI(t, test.account, http.MethodGet, "/app/bootstrap", "", nil)
			browserHostedStatus(t, response, test.status)
			var refusal appBootstrapRefusal
			browserHostedDecode(t, response, &refusal)
			if !test.csrf {
				if refusal.Details.CSRFToken != "" {
					t.Fatalf("anonymous refusal carries a CSRF token: %#v", refusal)
				}
				return
			}
			if refusal.Details.Organization != "org_browser_preview" || refusal.Details.CSRFToken != hostedCSRF(f.cookies[test.account].Value) {
				t.Fatalf("refusal = %#v", refusal)
			}
			start := f.rawAPI(t, test.account, http.MethodPost, browserHostedOrganizationBase+"/support/start", `{"idempotency_key":"support"}`, map[string]string{"X-CSRF-Token": refusal.Details.CSRFToken})
			want := http.StatusForbidden
			if test.account == "support-staff" {
				want = http.StatusOK
			}
			browserHostedStatus(t, start, want)
		})
	}
}
