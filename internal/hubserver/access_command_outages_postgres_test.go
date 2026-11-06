package hubserver

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresAccessCommandLookupOutages(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
	})
	for org, service := range f.services {
		repository := service.config.AccessRepository
		invitationID := "invitation_" + org
		project := tracker.ProjectID("project_" + org)
		email := "invitee_" + org + "@example.test"
		now := time.Now()
		provider := &browserHostedProvider{members: map[string]auth.Membership{}, invitations: map[string]auth.Invitation{invitationID: {ID: invitationID, Email: email, OrganizationID: "provider_" + org, State: "pending", ExpiresAt: now.Add(time.Hour)}}}
		member := auth.Membership{ID: org + "_owner", UserID: org + "_owner", OrganizationID: "provider_" + org, Status: "active"}
		member.Role.Slug = "owner"
		provider.members[member.ID] = member
		service.config.Hosted.Provider = provider
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: tracker.OrganizationID(org), ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: tracker.OrganizationID(org), UserID: org + "_owner", ProjectID: project, CanWrite: true, ManageRunner: true}); err != nil {
				return err
			}
			if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(org), TokenID: org + "_owner", ProjectID: project}); err != nil {
				return err
			}
			if err := w.CreateAccessInvitation(t.Context(), accessstore.Invitation{OrganizationID: tracker.OrganizationID(org), ID: invitationID, Email: email, Role: "member", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				return err
			}
			return w.CreateAccessInvitationGrant(t.Context(), accessstore.InvitationGrant{OrganizationID: tracker.OrganizationID(org), InvitationID: invitationID, ProjectID: project})
		}); err != nil {
			t.Fatal(err)
		}
		base := "/api/v2/organizations/" + org + "/members/"
		for _, route := range []struct {
			name, method, path, body, phase, stage string
			skip                                   int
		}{
			{"pending tenant", http.MethodPut, base + "invitations/" + invitationID, `{}`, "read", "tenant", 4},
			{"pending invitation", http.MethodPut, base + "invitations/" + invitationID, `{}`, "read", "invitation", 0},
			{"current invitation", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "invitation", 0},
			{"current project", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "project", 0},
			{"current project grant", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "project grant", 0},
			{"current token grant", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "token grant", 0},
			{"current principal grant", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "token grant", 1},
			{"replacement tenant", http.MethodPut, base + "invitations/" + invitationID, `{}`, "write", "tenant", 0},
			{"replacement project", http.MethodPut, base + "invitations/" + invitationID, `{"grants":[{"project_id":"` + string(project) + `","write":true}]}`, "write", "project", 1},
			{"replacement project grant", http.MethodPut, base + "invitations/" + invitationID, `{"grants":[{"project_id":"` + string(project) + `","write":true}]}`, "write", "project grant", 1},
			{"revoke tenant read", http.MethodDelete, base + "invitations/" + invitationID, ``, "read", "tenant", 4},
			{"revoke tenant write", http.MethodDelete, base + "invitations/" + invitationID, ``, "write", "tenant", 0},
			{"reserve tenant write", http.MethodPost, base + "invitations", `{"email":"new@example.test","role":"member","idempotency_key":"example-outage"}`, "write", "tenant", 0},
			{"role actor", http.MethodPut, base + member.ID + "/role", `{"role":"owner"}`, "read", "actor", 5},
		} {
			for _, failure := range []struct {
				name string
				err  error
				want int
			}{
				{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
				{"outage before missing", errors.Join(tenantauth.ErrUnavailable, corestore.ErrNotFound), http.StatusServiceUnavailable},
				{"outage before denial", errors.Join(tenantauth.ErrUnavailable, operatortool.ErrAccessDenied, corestore.ErrNotFound), http.StatusServiceUnavailable},
				{"missing", corestore.ErrNotFound, http.StatusForbidden},
			} {
				t.Run(org+"/"+route.name+"/"+failure.name, func(t *testing.T) {
					invitation := provider.invitations[invitationID]
					invitation.State = "pending"
					provider.invitations[invitationID] = invitation
					fault := &accessCommandFault{stage: route.stage, phase: route.phase, err: failure.err, skip: route.skip}
					service.config.AccessRepository = accessCommandFaultRepository{Repository: repository, fault: fault}
					defer func() { service.config.AccessRepository = repository }()
					response := hostedOutageRequest(t, f, org, route.method, route.path, route.body, "application/json")
					want := failure.want
					if failure.name == "missing" && route.stage == "invitation" {
						want = http.StatusNotFound
					}
					requireEntryAuthResponse(t, response.Code, response.Body.String(), want)
					if fault.calls <= fault.skip {
						t.Fatalf("fault not exercised: calls=%d skip=%d", fault.calls, fault.skip)
					}
				})
			}
		}
	}
}
