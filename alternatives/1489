package accessinvites_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresPendingInvitationRecoveryOutages(t *testing.T) {
	f := newAcceptanceFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		subject, id := "invitee_"+org, "pending_"+org
		email := subject + "@example.test"
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: tracker.OrganizationID(org), ID: id, Email: email, Role: "member", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name, stage string
			providerErr error
			want        int
		}{
			{"lookup outage", "invitations", nil, 503},
			{"provider outage", "", errors.New("private-outage-sentinel"), 503},
			{"no provider invitation", "", auth.ErrHostedIdentity, 403},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				service.config.AccessRepository = entryAuthFaultRepository{Repository: repository, stage: test.stage, err: errors.New("private-outage-sentinel")}
				defer func() { service.config.AccessRepository = repository }()
				service.config.Hosted.Provider = &invitationOutageProvider{entryAuthProvider: &entryAuthProvider{browserHostedProvider: &browserHostedProvider{}}, lookupErr: test.providerErr}
				response := f.request(t, org, "/organizations/"+org+"/app/bootstrap", "", func(c *cloudassert.Claims) {
					sharedRouteBrowserClaims(c)
					c.Method = http.MethodGet
					c.Subject, c.Email = subject, email
				})
				requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
			})
		}
	}
}
