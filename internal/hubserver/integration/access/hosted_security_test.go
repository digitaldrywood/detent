package access_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hostedTransactionCookie = "detent_hosted_login"

const nativeContractTestBody = "## Acceptance criteria\nThe change works.\n## Must not break\nExisting behavior.\n## How we know it worked\nThe focused tests pass."

func TestHostedSecurityRoleProjectMatrix(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"owner", "admin", "member", "viewer"} {
		for _, grant := range []string{"", "read", "write"} {
			t.Run(role+"/"+grant, func(t *testing.T) {
				t.Parallel()
				f := hubfixture.NewHostedSecurity(t)
				user := f.User(t, role, role, role+"@example.test", grant, "")
				if grant == "" {
					f.Revoke(t, user, f.Project)
				}
				readStatus := http.StatusNotFound
				if grant != "" {
					readStatus = http.StatusOK
				}
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base, nil), readStatus)
				writeStatus := http.StatusNotFound
				if grant != "" {
					writeStatus = http.StatusForbidden
				}
				if grant == "write" && role != "viewer" {
					writeStatus = http.StatusOK
				}
				command := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "new issue", Body: "private-body-sentinel" + hubfixture.IssueContractSections, State: "Todo"}
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPost, f.Base+"/work-items", command), writeStatus)
				policyStatus, revokeStatus := http.StatusNotFound, http.StatusNotFound
				if grant == "read" || grant == "write" && (role == "viewer" || role == "member") {
					policyStatus, revokeStatus = http.StatusForbidden, http.StatusForbidden
				}
				if grant == "write" && (role == "owner" || role == "admin") {
					policyStatus, revokeStatus = http.StatusOK, http.StatusGone
				}
				descriptor := hubfixture.Policy()
				for _, route := range []string{"/policy", "/onboarding/policy"} {
					hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPut, f.Base+route, policy.Change{Policy: descriptor}), policyStatus)
				}
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodDelete, f.Base+"/policy", map[string]string{"expected_policy_id": descriptor.ID}), revokeStatus)
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPost, f.Base+"/claims", map[string]any{}), http.StatusForbidden)
				runnerStatus := http.StatusNotFound
				if role == "owner" || role == "admin" {
					runnerStatus = http.StatusOK
				}
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), runnerStatus)
			})
		}
	}
}

func TestHostedSecuritySupportSessionConstraints(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		role   string
		grant  string
		change func(*testing.T, *hubfixture.Hosted, hubfixture.HostedUser)
		path   string
		want   int
	}{
		{name: "effective project read", role: "member", grant: "read", want: http.StatusOK},
		{name: "effective project write", role: "member", grant: "write", want: http.StatusOK},
		{name: "no effective project grant", role: "owner", want: http.StatusNotFound},
		{name: "viewer read", role: "viewer", grant: "write", want: http.StatusOK},
		{name: "wrong organization", role: "member", grant: "write", path: "/api/v2/organizations/org_other/projects/prj_security", want: http.StatusNotFound},
		{name: "expired provider session", role: "member", grant: "write", change: func(t *testing.T, f *hubfixture.Hosted, u hubfixture.HostedUser) {
			f.Provider.Mu.Lock()
			identity := f.Provider.Sessions[u.Identity.Hosted.SessionID]
			identity.ExpiresAt = time.Now().Add(-time.Minute)
			f.Provider.Sessions[identity.SessionID] = identity
			f.Provider.Mu.Unlock()
		}, want: http.StatusUnauthorized},
		{name: "revoked provider session", role: "member", grant: "write", change: func(t *testing.T, f *hubfixture.Hosted, u hubfixture.HostedUser) {
			if err := f.Provider.RevokeSession(t.Context(), u.Identity.Hosted.SessionID); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusUnauthorized},
		{name: "removed membership", role: "member", grant: "write", change: func(t *testing.T, f *hubfixture.Hosted, u hubfixture.HostedUser) {
			if err := f.Provider.RevokeMembership(t.Context(), "membership_"+u.Identity.Subject); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusForbidden},
		{name: "revoked local session", role: "member", grant: "write", change: func(t *testing.T, f *hubfixture.Hosted, u hubfixture.HostedUser) {
			if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessSession(t.Context(), apikey.HashToken(u.Token), f.Now().UTC())
			}); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusUnauthorized},
		{name: "changed support actor", role: "member", grant: "write", change: func(t *testing.T, f *hubfixture.Hosted, u hubfixture.HostedUser) {
			f.Provider.Mu.Lock()
			identity := f.Provider.Sessions[u.Identity.Hosted.SessionID]
			identity.SupportActor = "staff@example.test"
			f.Provider.Sessions[identity.SessionID] = identity
			f.Provider.Mu.Unlock()
		}, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, "customer", test.role, "customer@example.test", test.grant, "support@example.test")
			if test.grant == "" {
				f.Revoke(t, user, f.Project)
			}
			if test.change != nil {
				test.change(t, f, user)
			}
			path := test.path
			if path == "" {
				path = f.Base
			}
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, path, nil), test.want)
			if test.want == http.StatusOK {
				command := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "support-write"}, Title: "private-body-sentinel", State: "Todo"}
				wantWrite := http.StatusForbidden
				if test.role != "viewer" && test.grant == "write" {
					wantWrite = http.StatusOK
				}
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodPost, f.Base+"/work-items", command), wantWrite)
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, "/api/cloud/metadata", nil), http.StatusForbidden)
				if wantWrite != http.StatusOK {
					var count int
					if err := f.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND session_id=$2", f.Primary, user.Identity.Hosted.SessionID).Scan(&count); err != nil || count != 1 {
						t.Fatalf("support read audit rows=%d, want 1: %v", count, err)
					}
				}
				var auditID int64
				if err := f.Admin.QueryRowContext(t.Context(), "SELECT id FROM hosted_audit WHERE organization_id=$1 AND session_id=$2 ORDER BY id DESC LIMIT 1", f.Primary, user.Identity.Hosted.SessionID).Scan(&auditID); err != nil {
					t.Fatal(err)
				}
				var audit accessstore.Audit
				if err := f.Repository.ReadTenant(t.Context(), f.Primary, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					audit, err = r.AccessAudit(t.Context(), auditID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				actor, effective, organization, reason, raw := audit.ActualActor, audit.EffectiveUser, string(audit.OrganizationID), audit.Reason, audit.Event+audit.Route+string(audit.ProjectID)
				if actor != "support@example.test" || effective != user.Identity.Subject || organization != "org_security" || reason != "customer-request" || strings.Contains(raw, "private-body-sentinel") || strings.Contains(raw, user.Token) {
					t.Fatalf("audit actor=%q effective=%q organization=%q reason=%q", actor, effective, organization, reason)
				}
			}
		})
	}
}

func TestHostedSecurityCookieMutationsRequireCSRF(t *testing.T) {
	t.Parallel()
	for _, authorization := range []string{"", "Bearer invalid"} {
		t.Run(authorization, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, "owner", "owner", "owner@example.test", "", "")
			request := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(url.Values{"name": {"Rejected"}, "grant_access": {"true"}}.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Authorization", authorization)
			request.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: user.Token})
			response := httptest.NewRecorder()
			f.Handler().ServeHTTP(response, request)
			hubfixture.RequireStatus(t, response, http.StatusForbidden)
		})
	}
}

func TestHostedSecurityAdministrationRejectsBearerCookieMix(t *testing.T) {
	t.Parallel()
	for _, authorization := range []string{"Bearer invalid", "Bearer " + hubfixture.AdminToken} {
		for _, route := range []struct {
			method string
			path   string
			body   string
		}{
			{method: http.MethodPost, path: "/api/v2/organizations/org_security/projects", body: `{"idempotency_key":"mixed-auth","name":"Unauthorized project","states":[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]}`},
			{method: http.MethodPost, path: "/api/v2/organizations/org_security/runner-enrollments", body: `{}`},
			{method: http.MethodGet, path: "/api/v2/organizations/org_security/runners"},
			{method: http.MethodPut, path: "/api/v2/organizations/org_security/runners/runner_unknown/routing", body: `{}`},
		} {
			t.Run(authorization+"/"+route.method+route.path, func(t *testing.T) {
				t.Parallel()
				f := hubfixture.NewHostedSecurity(t)
				user := f.User(t, "owner", "owner", "owner@example.test", "write", "")
				f.Grant(t, user, f.Project, true, true)
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", authorization)
				request.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: user.Token})
				response := httptest.NewRecorder()
				f.Handler().ServeHTTP(response, request)
				hubfixture.RequireStatus(t, response, http.StatusNotFound)
				var count int
				if err := f.Admin.QueryRowContext(t.Context(), "SELECT count(*) FROM projects WHERE organization_id=$1", f.Primary).Scan(&count); err != nil || count != 1 {
					t.Fatalf("mixed authentication changed projects: count=%d, error=%v", count, err)
				}
			})
		}
	}
}

func TestHostedSecurityReplacementMembershipRequiresNewGrants(t *testing.T) {
	t.Parallel()
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%v", replacement), func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, "member", "member", "member@example.test", "write", "")
			f.Grant(t, user, f.Project, true, true)
			f.Provider.Mu.Lock()
			membership := f.Provider.Members["membership_"+user.Identity.Subject]
			if replacement {
				delete(f.Provider.Members, membership.ID)
				membership.ID += "_replacement"
				f.Provider.Members[membership.ID] = membership
			}
			f.Provider.Mu.Unlock()
			f.AddMember(t, user.Identity, membership)
			want := http.StatusOK
			if replacement {
				want = http.StatusNotFound
			}
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base, nil), want)
			hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), want)
			if replacement {
				f.Grant(t, user, f.Project, false, false)
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base, nil), http.StatusOK)
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), http.StatusNotFound)
			}
		})
	}
}

func TestHostedSecuritySSEDeniesUnscopedProject(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	user := f.User(t, "viewer", "viewer", "viewer@example.test", "", "")
	response := f.Request(t, user, http.MethodGet, "/projects/"+string(f.Project)+"/events", nil)
	if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
		t.Fatalf("ungranted project stream returned %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "event:") {
		t.Fatal("ungranted project emitted an event")
	}
}

func TestHostedSecurityLoginTransactions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, start, email string
		wrongState         bool
		want               int
	}{
		{name: "customer provider organization", start: "/auth/oidc/start", email: "owner@example.test", want: http.StatusSeeOther},
		{name: "staff without organization", start: "/auth/oidc/start?staff=1", email: "staff@example.test", want: http.StatusSeeOther},
		{name: "wrong callback state", start: "/auth/oidc/start", email: "owner@example.test", wrongState: true, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, "owner", "owner", test.email, "", "")
			f.Provider.Mu.Lock()
			f.Provider.Codes["code"] = user.Identity
			f.Provider.Mu.Unlock()
			start := f.Request(t, hubfixture.HostedUser{}, http.MethodGet, test.start, nil)
			hubfixture.RequireStatus(t, start, http.StatusSeeOther)
			location, err := url.Parse(start.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			state := location.Query().Get("state")
			if state == "" {
				t.Fatal("missing login state")
			}
			if test.wrongState {
				state = "wrong-state"
			}
			var cookie *http.Cookie
			for _, current := range start.Result().Cookies() {
				if current.Name == hostedTransactionCookie {
					cookie = current
				}
			}
			if cookie == nil {
				t.Fatal("missing transaction cookie")
			}
			path := "/auth/oidc/callback?" + url.Values{"state": {state}, "code": {"code"}}.Encode()
			for attempt := range 2 {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.AddCookie(cookie)
				response := httptest.NewRecorder()
				f.Handler().ServeHTTP(response, request)
				want := test.want
				if attempt == 1 {
					want = http.StatusUnauthorized
				}
				hubfixture.RequireStatus(t, response, want)
			}
			f.Provider.Mu.Lock()
			exchanges := f.Provider.Exchanges
			f.Provider.Mu.Unlock()
			wantExchanges := 1
			if test.wrongState {
				wantExchanges = 0
			}
			if exchanges != wantExchanges {
				t.Fatalf("provider exchanges = %d, want %d", exchanges, wantExchanges)
			}
		})
	}
}

func TestHostedSecurityRoleRevocationAppliesImmediately(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	user := f.User(t, "member", "member", "member@example.test", "write", "")
	command := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "before-role-change"}, Title: "private-mutation-sentinel", State: "Todo"}
	response := f.Request(t, user, http.MethodPost, f.Base+"/work-items", command)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	replay := f.Request(t, user, http.MethodPost, f.Base+"/work-items", command)
	hubfixture.RequireStatus(t, replay, http.StatusOK)
	if response.Body.String() != replay.Body.String() {
		t.Fatal("idempotent replay changed the mutation response")
	}
	if err := f.Provider.SetMembershipRole(t.Context(), "membership_"+user.Identity.Subject, "viewer"); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base, nil), http.StatusOK)
	response = f.Request(t, user, http.MethodPost, f.Base+"/work-items", command)
	hubfixture.RequireStatus(t, response, http.StatusForbidden)
	if strings.Contains(response.Body.String(), "private-mutation-sentinel") {
		t.Fatal("role downgrade exposed cached mutation")
	}
}

func TestHostedSecuritySupportLoginAndExit(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t)
	staff := f.User(t, "support", "owner", "support@example.test", "", "")
	customer := f.User(t, "customer", "viewer", "customer@example.test", "read", "support@example.test")
	start := f.Request(t, staff, http.MethodPost, "/support/start", url.Values{})
	hubfixture.RequireStatus(t, start, http.StatusOK)
	var transaction *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == hostedTransactionCookie {
			transaction = cookie
		}
	}
	if transaction == nil {
		t.Fatal("support login transaction cookie was not set")
	}
	f.Provider.Mu.Lock()
	f.Provider.Codes["support-code"] = customer.Identity
	f.Provider.Mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=support-code", nil)
	request.AddCookie(transaction)
	request.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: staff.Token})
	callback := httptest.NewRecorder()
	f.Handler().ServeHTTP(callback, request)
	hubfixture.RequireStatus(t, callback, http.StatusSeeOther)
	var sessionCookie *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == hubfixture.HostedCookie {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("support session cookie was not set")
	}
	support := hubfixture.HostedUser{Identity: customer.Identity, Token: sessionCookie.Value}
	page := f.Request(t, support, http.MethodGet, "/organization", nil)
	hubfixture.RequireStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "support@example.test") || !strings.Contains(page.Body.String(), "/logout") {
		t.Fatal("support indicator or exit flow is missing")
	}
	hubfixture.RequireStatus(t, f.Request(t, support, http.MethodPost, "/logout", url.Values{}), http.StatusSeeOther)
	hubfixture.RequireStatus(t, f.Request(t, support, http.MethodGet, f.Base, nil), http.StatusUnauthorized)
	if _, err := f.Provider.CurrentSession(t.Context(), *customer.Identity.Hosted); !errors.Is(err, auth.ErrHostedIdentity) {
		t.Fatalf("provider support session was not revoked: %v", err)
	}
	var starts, ends int
	err := f.Admin.QueryRowContext(t.Context(), "SELECT count(*) FILTER(WHERE event='session_started'),count(*) FILTER(WHERE event='session_ended') FROM hosted_audit WHERE session_id=$1 AND actual_actor=$2 AND effective_user=$3 AND organization_id=$4", customer.Identity.Hosted.SessionID, "support@example.test", customer.Identity.Subject, f.Primary).Scan(&starts, &ends)
	if err != nil || starts != 1 || ends != 1 {
		t.Fatalf("support lifecycle audit starts=%d ends=%d error=%v", starts, ends, err)
	}
}

func TestHostedRepeatedReadsAreNotAudited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		support string
	}{
		{name: "member reads"},
		{name: "support reads", support: "support@example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := hubfixture.NewHostedSecurity(t)
			user := f.User(t, "customer", "member", "customer@example.test", "read", test.support)
			for range 3 {
				hubfixture.RequireStatus(t, f.Request(t, user, http.MethodGet, f.Base, nil), http.StatusOK)
			}
			var count, attributed int
			if err := f.Admin.QueryRowContext(t.Context(), "SELECT count(*),count(*) FILTER(WHERE actual_actor=$1 AND effective_user=$2 AND reason=$3) FROM hosted_audit WHERE session_id=$4 AND event='action' AND route LIKE 'GET %' AND organization_id=$5", test.support, user.Identity.Subject, user.Identity.Hosted.SupportReason, user.Identity.Hosted.SessionID, f.Primary).Scan(&count, &attributed); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.support != "" {
				want = 3
			}
			if count != want || attributed != want {
				t.Fatalf("read audit rows=%d attributed=%d, want %d", count, attributed, want)
			}
		})
	}
}
