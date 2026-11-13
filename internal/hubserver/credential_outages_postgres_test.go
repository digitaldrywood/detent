package hubserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresPersonalCredentialOutages(t *testing.T) {
	t.Parallel()
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		provider := &entryAuthProvider{browserHostedProvider: &browserHostedProvider{members: map[string]auth.Membership{}}}
		member := auth.Membership{ID: org + "_owner", UserID: org + "_owner", OrganizationID: "provider_" + org, Status: "active"}
		member.Role.Slug = "owner"
		provider.members[member.ID] = member
		service.config.Hosted.Provider = provider
		token := "personal_" + org
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: tracker.OrganizationID(org), ID: token, Name: token, Hash: apikey.HashToken(token), Scope: "operator", Kind: "personal", ProjectAccess: "all", UserID: member.UserID, MembershipID: member.ID, KeyScope: "read", CreatedAt: time.Now(), UpdatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/organizations/" + org + "/health", "/api/v2/organizations/" + org + "/projects/prj_absent"} {
			for _, test := range []struct {
				name, status, id string
				err              error
				want             int
			}{
				{"provider outage", "active", member.ID, errors.New("private-outage-sentinel"), 503},
				{"provider unavailable", "active", member.ID, &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, 503},
				{"mismatched membership", "active", "other_membership", nil, 401},
				{"removed membership", "inactive", member.ID, nil, 401},
				{"valid identity", "active", member.ID, nil, 404},
			} {
				t.Run(org+path+"/"+test.name, func(t *testing.T) {
					provider.membershipErr = test.err
					current := member
					current.ID, current.Status = test.id, test.status
					provider.members = map[string]auth.Membership{current.ID: current}
					response := f.request(t, org, path, "", func(c *cloudassert.Claims) {
						c.Kind = cloudassert.KindMachine
						c.Method = http.MethodGet
						c.Subject, c.Email, c.Role = "", "", ""
					}, map[string]string{"Authorization": "Bearer " + token})
					want := test.want
					if test.name == "valid identity" && path != "/organizations/"+org+"/health" {
						want = http.StatusNotFound
					}
					requireEntryAuthResponse(t, response.Code, response.Body.String(), want)
				})
			}
		}
	}
}

func (r entryAuthFaultReader) AccessInvitations(ctx context.Context, after string, limit int) ([]accessstore.Invitation, error) {
	if r.stage == "invitations" {
		return nil, r.err
	}
	return r.Reader.AccessInvitations(ctx, after, limit)
}
