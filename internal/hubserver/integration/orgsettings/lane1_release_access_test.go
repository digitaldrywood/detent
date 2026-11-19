package orgsettings_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestReleaseNotesAccess(t *testing.T) {
	t.Parallel()
	h := hubfixture.NewHostedSecurity(t)
	for _, test := range []struct{ name, user, path string }{
		{"unauthenticated", "", "/api/v2/organizations/org_security/release-notes"},
		{"wrong organization", "member", "/api/v2/organizations/org_other/release-notes"},
		{"revoked member", "revoked", "/api/v2/organizations/org_security/release-notes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			user := hubfixture.HostedUser{}
			if test.user != "" {
				user = h.User(t, test.user, "member", "member@example.test", "read", "")
			}
			if test.user == "revoked" {
				if err := h.Provider.RevokeMembership(t.Context(), "membership_"+user.Identity.Subject); err != nil {
					t.Fatal(err)
				}
			}
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				path := test.path
				if method == http.MethodPut {
					path += "/seen"
				}
				response := h.Request(t, user, method, path, nil)
				if response.Code == http.StatusOK {
					t.Fatalf("unauthorized %s succeeded", method)
				}
			}
		})
	}
}
