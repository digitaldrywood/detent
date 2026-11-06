package hubserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestPostgresOrganizationMembersAndSecretRoutes(t *testing.T) {
	repository := projectRecordPostgres(t)
	providerCalls := 0
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.SpritesHTTPClient = &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
			providerCalls++
			return spritesTestResponse(`{"sprites":[]}`, http.StatusOK), nil
		})}
	})
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	member := f.user(t, "member", "member", "member@example.test", "", "")
	disableLegacyPostgresTestHandles(t, f.service, repository)
	base := "/api/v2/organizations/org_security"
	for _, test := range []struct {
		user hostedSecurityUser
		want int
	}{{owner, 2}, {member, 1}} {
		response := f.request(t, test.user, http.MethodGet, base+"/members", nil)
		requireNativeStatus(t, response, http.StatusOK)
		var members hostedMembersResponse
		decodeHubResponse(t, response, &members)
		if members.MemberCount != 2 || len(members.Members) != test.want {
			t.Fatalf("member visibility: count=%d visible=%d, want 2/%d", members.MemberCount, len(members.Members), test.want)
		}
		if test.want == 1 && members.Members[0].UserID != member.identity.Subject {
			t.Fatal("member received another member's account")
		}
	}
	path := base + "/secrets/" + flySpritesToken
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, path, nil), http.StatusOK)
	input := map[string]any{"idempotency_key": "example-secret", "token": spritesSecretSentinel}
	requireNativeStatus(t, f.request(t, member, http.MethodPut, path, input), http.StatusForbidden)
	if providerCalls != 0 {
		t.Fatal("unauthorized mutation reached the provider")
	}
	response := f.request(t, owner, http.MethodPut, path, input)
	requireNativeStatus(t, response, http.StatusOK)
	if providerCalls != 1 || strings.Contains(response.Body.String(), spritesSecretSentinel) {
		t.Fatal("secret write did not validate once and return metadata only")
	}
	response = f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var status projectSecretStatus
	decodeHubResponse(t, response, &status)
	if !status.Present || status.KeyVersion != 1 || strings.Contains(response.Body.String(), spritesSecretSentinel) {
		t.Fatal("secret metadata did not preserve presence without its value")
	}
	requireNativeStatus(t, f.request(t, member, http.MethodDelete, path, nil), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodDelete, path, nil), http.StatusNoContent)
	response = f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &status)
	if status.Present {
		t.Fatal("deleted organization secret remains present")
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), owner.identity.Subject, now.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base + "/members", path} {
		requireNativeStatus(t, f.request(t, owner, http.MethodGet, path, nil), http.StatusForbidden)
	}
}
