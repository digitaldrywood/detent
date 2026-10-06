//go:build !windows

package cloudentry

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
)

func pilotSupportSession(t *testing.T, p *sharedOriginPilot, organization string) *pilotBrowser {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	hosted := auth.HostedIdentity{Subject: "user_dana", OrganizationID: "porg_" + organization, SessionID: "impersonation_" + organization, CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour), SupportActor: "staff@example.test", SupportReason: "troubleshooting"}
	p.provider.mu.Lock()
	p.provider.sessions[hosted.SessionID] = hosted
	p.provider.mu.Unlock()
	token := "impersonation-token-" + organization
	if err := p.service.auth.createSession(t.Context(), apikey.HashToken(token), "impersonation-csrf", auth.Identity{Subject: "user_dana", Email: "staff@example.test", EmailVerified: true, Hosted: &hosted}); err != nil {
		t.Fatal(err)
	}
	b := p.browser(t)
	base, err := url.Parse(p.base)
	if err != nil {
		t.Fatal(err)
	}
	b.client.Jar.SetCookies(base, []*http.Cookie{{Name: p.service.cookieName("session"), Value: token, Path: "/"}})
	return b
}

type platformListing struct {
	CSRF     string `json:"csrf"`
	CanGrant bool   `json:"can_grant"`
}

func TestPlatformComplimentaryPlansThroughTenantHub(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	for _, test := range []struct {
		name, feature, plan string
	}{
		{"complimentary plan", "", "pilot_plus"},
		{"model choice", "model_choice", "pilot_free"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPlatformEntitlement(t, test.feature, test.plan)
		})
	}
}

func testPlatformEntitlement(t *testing.T, feature, planID string) {
	t.Helper()
	p := newSharedOriginPilot(t, 2, 1)
	p.provider.users["user_staff"] = "staff@example.test"
	p.provider.users["user_ops"] = "ops@example.test"
	dana := p.browser(t)
	pilotStatus(t, "dana sign-in", dana.login("/auth/oidc/start", "user_dana:"), http.StatusSeeOther)
	created := dana.createOrganization("Alpha Labs")
	pilotStatus(t, "dana create", created, http.StatusSeeOther)
	organization := organizationFromLocation(t, created.location)
	p.waitState(t, organization, "ready")
	path := "/api/cloud/platform/organizations/" + organization + "/entitlements"

	admin, ops, anonymous := p.browser(t), p.browser(t), p.browser(t)
	pilotStatus(t, "admin sign-in", admin.login("/auth/oidc/start", "user_staff:"), http.StatusSeeOther)
	pilotStatus(t, "ops sign-in", ops.login("/auth/oidc/start", "user_ops:"), http.StatusSeeOther)
	support := pilotSupportSession(t, p, organization)
	csrf := map[*pilotBrowser]string{}
	for _, test := range []struct {
		name     string
		browser  *pilotBrowser
		canGrant bool
	}{{"administrator", admin, true}, {"other staff", ops, false}} {
		var listing platformListing
		response := test.browser.get("/api/cloud/platform/organizations")
		pilotStatus(t, test.name+" listing", response, http.StatusOK)
		pilotDecode(t, response, &listing)
		if listing.CanGrant != test.canGrant || listing.CSRF == "" {
			t.Fatalf("%s listing = %+v", test.name, listing)
		}
		csrf[test.browser] = listing.CSRF
	}

	var state organizationEntitlements
	response := admin.get(path)
	pilotStatus(t, "administrator reads entitlements", response, http.StatusOK)
	pilotDecode(t, response, &state)
	if state.OrganizationID != organization || state.Base.ID != "pilot_free" || state.EffectiveBase.ID != "pilot_free" || len(state.Grants) != 0 || len(state.Plans) != 2 || state.Revision < 1 || slices.Contains(state.Features, "model_choice") {
		t.Fatalf("initial entitlements = %+v", state)
	}
	if strings.Contains(response.body, pilotOperatorToken) {
		t.Fatal("entitlements response exposed the tenant credential")
	}
	revision := state.Revision
	expires := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	grant := entitlementChange{Action: "grant", IdempotencyKey: "comp-alpha-1", ExpectedRevision: revision, Plan: planReference{ID: "pilot_plus", Version: 1}, ExpiresAt: &expires, Reason: "design partner"}
	if feature != "" {
		grant.Feature, grant.Plan = feature, planReference{}
	}

	for _, test := range []struct {
		name    string
		browser *pilotBrowser
		csrf    string
		want    int
	}{
		{"anonymous", anonymous, "", http.StatusUnauthorized},
		{"customer owner", dana, "", http.StatusForbidden},
		{"support session", support, "impersonation-csrf", http.StatusForbidden},
		{"other staff", ops, csrf[ops], http.StatusForbidden},
		{"administrator without csrf", admin, "", http.StatusForbidden},
		{"administrator with another csrf", admin, csrf[ops], http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			pilotStatus(t, test.name+" read", test.browser.get(path), map[bool]int{true: http.StatusOK, false: test.want}[test.browser == admin])
			refused := test.browser.json(http.MethodPost, path, test.csrf, grant)
			pilotStatus(t, test.name+" grant", refused, test.want)
			if strings.Contains(refused.body, "pilot_plus") {
				t.Fatalf("refusal leaked plan data: %s", refused.body)
			}
		})
	}
	var changes int
	if err := p.service.registry.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM entitlement_changes").Scan(&changes); err != nil || changes != 0 {
		t.Fatalf("refused requests recorded %d changes (%v)", changes, err)
	}

	missingReason := grant
	missingReason.Reason = "  "
	pilotStatus(t, "grant without reason", admin.json(http.MethodPost, path, csrf[admin], missingReason), http.StatusUnprocessableEntity)
	unknownPlan := grant
	unknownPlan.Plan = planReference{ID: "enterprise", Version: 1}
	pilotStatus(t, "grant of an unconfigured plan", admin.json(http.MethodPost, path, csrf[admin], unknownPlan), http.StatusUnprocessableEntity)
	unknownFeature := grant
	unknownFeature.Feature, unknownFeature.Plan = "unknown_feature", planReference{}
	pilotStatus(t, "grant of an unknown feature", admin.json(http.MethodPost, path, csrf[admin], unknownFeature), http.StatusUnprocessableEntity)

	var first, retry map[string]string
	response = admin.json(http.MethodPost, path, csrf[admin], grant)
	pilotStatus(t, "grant", response, http.StatusOK)
	pilotDecode(t, response, &first)
	response = admin.json(http.MethodPost, path, csrf[admin], grant)
	pilotStatus(t, "idempotent grant retry", response, http.StatusOK)
	pilotDecode(t, response, &retry)
	if first["grant_id"] == "" || first["grant_id"] != retry["grant_id"] {
		t.Fatalf("grant = %v, retry = %v", first, retry)
	}

	response = admin.get(path)
	pilotStatus(t, "entitlements after grant", response, http.StatusOK)
	pilotDecode(t, response, &state)
	if len(state.Grants) != 1 || state.Revision != revision+1 || slices.Contains(state.Features, "model_choice") != (feature != "") {
		t.Fatalf("entitlements after grant = %+v", state)
	}
	granted := state.Grants[0]
	if granted.ID != first["grant_id"] || granted.Plan.ID != planID || granted.Reason != "design partner" || granted.GrantedBy != "staff@example.test" || granted.ExpiresAt == nil || !granted.ExpiresAt.Equal(expires) || len(granted.Scope) == 0 {
		t.Fatalf("grant = %+v", granted)
	}
	if feature != "" && !slices.Equal(granted.Scope, []string{"model_choice"}) {
		t.Fatalf("feature grant scope = %v", granted.Scope)
	}

	stale := grant
	stale.IdempotencyKey = "comp-alpha-2"
	response = admin.json(http.MethodPost, path, csrf[admin], stale)
	pilotStatus(t, "stale revision", response, http.StatusConflict)
	if !strings.Contains(response.body, "revision_conflict") || !strings.Contains(response.body, "reload and try again") {
		t.Fatalf("stale revision body = %s", response.body)
	}

	revoke := entitlementChange{Action: "revoke", IdempotencyKey: "comp-alpha-revoke", ExpectedRevision: state.Revision, GrantID: granted.ID}
	pilotStatus(t, "revoke without reason", admin.json(http.MethodPost, path, csrf[admin], revoke), http.StatusUnprocessableEntity)
	revoke.Reason = "pilot ended"
	for _, browser := range []*pilotBrowser{dana, ops, support} {
		pilotStatus(t, "non-administrator revoke", browser.json(http.MethodPost, path, csrf[browser], revoke), http.StatusForbidden)
	}
	pilotStatus(t, "revoke", admin.json(http.MethodPost, path, csrf[admin], revoke), http.StatusOK)
	response = admin.get(path)
	pilotDecode(t, response, &state)
	if len(state.Grants) != 0 || state.Revision != revision+2 || slices.Contains(state.Features, "model_choice") {
		t.Fatalf("entitlements after revoke = %+v", state)
	}

	rows, err := p.service.registry.store.db.QueryContext(t.Context(), "SELECT action,grant_id,plan_id,plan_version,expires_at,reason,staff_email FROM entitlement_changes WHERE organization_id = ? ORDER BY recorded_at, action", organization)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var audit []string
	for rows.Next() {
		var action, grantID, plan, expiry, reason, email string
		var version int64
		if err := rows.Scan(&action, &grantID, &plan, &version, &expiry, &reason, &email); err != nil {
			t.Fatal(err)
		}
		if grantID != granted.ID || plan != planID || version != 1 || expiry != formatTime(expires) || email != "staff@example.test" {
			t.Fatalf("audit row = %s %s %s %d %s %s %s", action, grantID, plan, version, expiry, reason, email)
		}
		audit = append(audit, action+":"+reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(audit, ",") != "grant:design partner,revoke:pilot ended" {
		t.Fatalf("audit = %v", audit)
	}
	if got := platformAuditCount(t, p.service, "user_staff", "platform_entitlement_change_requested"); got < 4 {
		t.Fatalf("staff change audit = %d", got)
	}
}
