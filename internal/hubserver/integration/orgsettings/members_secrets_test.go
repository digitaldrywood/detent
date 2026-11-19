package orgsettings_test

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type organizationSecretTransport func(*http.Request) (*http.Response, error)

func (transport organizationSecretTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestPostgresOrganizationMembersAndSecretRoutes(t *testing.T) {
	t.Parallel()
	const secretKind = "fly_sprites_token"
	const secretValue = "detent-test/org-id/token-id/SPRITES-VALUE-SENTINEL"
	providerReached := false
	f := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.SpritesHTTPClient = &http.Client{Transport: organizationSecretTransport(func(*http.Request) (*http.Response, error) {
			providerReached = true
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"sprites":[]}`)), Header: make(http.Header)}, nil
		})}
	})
	owner := f.User(t, "owner", "owner", "owner@example.test", "", "")
	member := f.User(t, "member", "member", "member@example.test", "", "")
	base := hubfixture.OrganizationBase(f.Primary)
	for _, test := range []struct {
		user hubfixture.HostedUser
		want []string
	}{{owner, []string{owner.Identity.Subject, member.Identity.Subject}}, {member, []string{member.Identity.Subject}}} {
		response := f.Request(t, test.user, http.MethodGet, base+"/members", nil)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var body struct {
			Members []struct {
				UserID string `json:"user_id"`
			} `json:"members"`
		}
		hubfixture.Decode(t, response, &body)
		var visible []string
		for _, account := range body.Members {
			visible = append(visible, account.UserID)
		}
		slices.Sort(visible)
		slices.Sort(test.want)
		if !slices.Equal(visible, test.want) {
			t.Fatalf("visible members = %v, want %v", visible, test.want)
		}
	}
	type secretStatus struct {
		Kind       string `json:"kind"`
		Present    bool   `json:"present"`
		KeyVersion int    `json:"key_version"`
	}
	path := base + "/secrets/" + secretKind
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodGet, path, nil), http.StatusOK)
	input := map[string]any{"idempotency_key": "example-secret", "token": secretValue}
	hubfixture.RequireStatus(t, f.Request(t, member, http.MethodPut, path, input), http.StatusForbidden)
	if providerReached {
		t.Fatal("unauthorized mutation reached the provider")
	}
	response := f.Request(t, owner, http.MethodPut, path, input)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	if !providerReached || strings.Contains(response.Body.String(), secretValue) {
		t.Fatal("secret write did not validate and return metadata only")
	}
	response = f.Request(t, owner, http.MethodGet, path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var status secretStatus
	hubfixture.Decode(t, response, &status)
	if !status.Present || status.KeyVersion != 1 || strings.Contains(response.Body.String(), secretValue) {
		t.Fatal("secret metadata did not preserve presence without its value")
	}
	hubfixture.RequireStatus(t, f.Request(t, member, http.MethodDelete, path, nil), http.StatusForbidden)
	response = f.Request(t, owner, http.MethodDelete, path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var removed secretStatus
	hubfixture.Decode(t, response, &removed)
	if removed.Kind != secretKind || removed.Present || strings.Contains(response.Body.String(), secretValue) {
		t.Fatal("secret deletion did not return absent metadata without its value")
	}
	response = f.Request(t, owner, http.MethodGet, path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &status)
	if status.Present {
		t.Fatal("deleted organization secret remains present")
	}
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessMember(t.Context(), owner.Identity.Subject, f.Now().Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{base + "/members", path} {
		hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodGet, path, nil), http.StatusForbidden)
	}
}
