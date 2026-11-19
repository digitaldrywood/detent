package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

const conversationFixtureDirectory = "../../web/conversation/src/contracts/fixtures"

func TestHostedAccountAPI(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
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
		{name: "support viewer plan", account: "support-viewer", method: http.MethodGet, path: "/plan", status: http.StatusForbidden, code: "forbidden"},
		{name: "wrong organization plan", account: "wrong-organization", method: http.MethodGet, path: "/plan", status: http.StatusForbidden, code: "forbidden"},
		{name: "revoked session plan", account: "revoked", method: http.MethodGet, path: "/plan", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "expired session plan", account: "expired", method: http.MethodGet, path: "/plan", status: http.StatusUnauthorized, code: "unauthorized"},
		{name: "owner plan", account: "owner", method: http.MethodGet, path: "/plan", status: http.StatusOK, check: func(t *testing.T, body []byte) {
			var plan hostedplans.HostedEntitlement
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
			var failure hubapi.APIErrorResponse
			browserHostedDecode(t, response, &failure)
			if test.code != "" && failure.Code != test.code || failure.Message == "" {
				t.Fatalf("error = %#v, want code %q", failure, test.code)
			}
		})
	}
	t.Run("another organization is not found", func(t *testing.T) {
		response := f.rawAPI(t, "owner", http.MethodGet, "/api/v2/organizations/org_other/plan", "", nil)
		browserHostedStatus(t, response, http.StatusNotFound)
	})
	t.Run("support start", func(t *testing.T) {
		for _, test := range []struct {
			account string
			status  int
		}{{"", http.StatusForbidden}, {"owner", http.StatusForbidden}, {"staff", http.StatusForbidden}, {"support-staff", http.StatusOK}} {
			t.Run(test.account, func(t *testing.T) {
				if test.account == "" {
					browserHostedStatus(t, f.rawAPI(t, "", http.MethodPost, browserHostedOrganizationBase+"/support/start", `{"idempotency_key":"support"}`, nil), test.status)
					return
				}
				f.api(t, test.account, http.MethodPost, browserHostedOrganizationBase+"/support/start", map[string]string{"idempotency_key": "support"}, test.status)
			})
		}
	})
	t.Run("accept invitation", func(t *testing.T) {
		var invitation hostedInvitationView
		browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/members/invitations", map[string]string{"email": "invitee@example.test", "role": "member", "idempotency_key": "invite"}, http.StatusCreated), &invitation)
		for _, test := range []struct {
			account string
			status  int
		}{{"", http.StatusForbidden}, {"owner", http.StatusForbidden}, {"invitee", http.StatusOK}, {"invitee", http.StatusOK}} {
			t.Run(test.account, func(t *testing.T) {
				if test.account == "" {
					browserHostedStatus(t, f.rawAPI(t, "", http.MethodPost, browserHostedOrganizationBase+"/invitations/accept", `{"token":"`+invitation.ID+`","idempotency_key":"accept"}`, nil), test.status)
					return
				}
				f.api(t, test.account, http.MethodPost, browserHostedOrganizationBase+"/invitations/accept", map[string]string{"token": invitation.ID, "idempotency_key": "accept"}, test.status)
			})
		}
		var subject string
		if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT accepted_user_id FROM hosted_invitations WHERE organization_id=$1 AND id=$2`, f.service.accessOrganization(), invitation.ID).Scan(&subject); err != nil || subject != "user_browser_invitee" {
			t.Fatalf("accepted subject=%q error=%v", subject, err)
		}
	})
}

func TestHostedSharedAccountAPI(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t, withAppClientFS(appClientBundle()))
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

		})
	}
}
