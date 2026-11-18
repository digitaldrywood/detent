package accessinvites_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessinvites"
	"github.com/digitaldrywood/detent/internal/hubserver/accessmembers"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type browserHostedProvider struct {
	auth.HostedProvider
	mu          sync.Mutex
	members     map[string]auth.Membership
	invitations map[string]auth.Invitation
	inviteRoles map[string]string
}

func (p *browserHostedProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var members []auth.Membership
	for _, member := range p.members {
		if (user == "" || member.UserID == user) && (organization == "" || member.OrganizationID == organization) {
			members = append(members, member)
		}
	}
	return members, nil
}

func (p *browserHostedProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *browserHostedProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.invitations[token] = invitation
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: invitation.OrganizationID, Status: "active"}
	member.Role.Slug = p.inviteRoles[token]
	p.members[member.ID] = member
	return nil
}

func (p *browserHostedProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}

func (p *browserHostedProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func (p *browserHostedProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = role
	p.members[member.ID] = member
	return member, nil
}

type entryAuthProvider struct {
	*browserHostedProvider
	membershipErr error
}

func (p *entryAuthProvider) Memberships(ctx context.Context, user, org string) ([]auth.Membership, error) {
	if p.membershipErr != nil {
		return nil, p.membershipErr
	}
	return p.browserHostedProvider.Memberships(ctx, user, org)
}

type entryAuthFaultRepository struct {
	tenantstore.Repository
	stage string
	err   error
}

func (r entryAuthFaultRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, org, workload, func(reader tenantstore.Reader) error {
		return read(entryAuthFaultReader{Reader: reader, stage: r.stage, err: r.err})
	})
}
func (r entryAuthFaultRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.stage == "write" {
		return r.err
	}
	return r.Repository.WriteTenant(ctx, org, workload, write)
}

type entryAuthFaultReader struct {
	tenantstore.Reader
	stage string
	err   error
}

func (r entryAuthFaultReader) AccessTenant(ctx context.Context) (accessstore.Tenant, error) {
	if r.stage == "tenant" {
		return accessstore.Tenant{}, r.err
	}
	return r.Reader.AccessTenant(ctx)
}
func (r entryAuthFaultReader) AccessInvitations(ctx context.Context, after string, limit int) ([]accessstore.Invitation, error) {
	if r.stage == "invitations" {
		return nil, r.err
	}
	return r.Reader.AccessInvitations(ctx, after, limit)
}

type acceptanceConfig struct {
	AccessRepository tenantstore.Repository
	Hosted           *acceptanceHosted
}
type acceptanceHosted struct{ Provider auth.HostedProvider }
type acceptanceService struct {
	config       acceptanceConfig
	organization tracker.OrganizationID
	harness      *hubtest.HTTPHarness
}

func (s *acceptanceService) authenticator() *hubauth.Authenticator {
	a := hubauth.New(hubauth.Deps{AccessRepository: s.config.AccessRepository, Organization: s.organization, Now: time.Now, Memberships: s.config.Hosted.Provider.Memberships, Hosted: &hubauth.Hosted{OrganizationID: string(s.organization), Shared: true}, SharedClaims: func(c echo.Context) (cloudassert.Claims, bool) {
		claims, ok := c.Get("claims").(cloudassert.Claims)
		return claims, ok
	}, SharedSession: func(ctx context.Context, c echo.Context) (auth.Session, string, error) {
		claims, _ := c.Get("claims").(cloudassert.Claims)
		return s.authenticator().SharedAccessSession(ctx, c, claims)
	}, CompleteInvitation: func(ctx context.Context, session auth.Session) (bool, error) {
		return s.feature().RecoverPending(ctx, session)
	}})
	return a
}
func (s *acceptanceService) feature() *accessinvites.Service {
	m := accessmembers.New(accessmembers.Deps{Repository: s.config.AccessRepository, Organization: s.organization, Now: time.Now, GenerateToken: apikey.GenerateToken})
	return accessinvites.New(accessinvites.Deps{Repository: s.config.AccessRepository, Organization: s.organization, Now: time.Now, Provider: s.config.Hosted.Provider, Auth: s.authenticator(), Members: m})
}

type acceptanceFixture struct{ services map[string]*acceptanceService }

func newAcceptanceFixture(t *testing.T) acceptanceFixture {
	t.Helper()
	repository := hubtest.Repository(t)
	f := acceptanceFixture{services: map[string]*acceptanceService{}}
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: org})
		s := &acceptanceService{organization: org, config: acceptanceConfig{AccessRepository: repository, Hosted: &acceptanceHosted{}}}
		s.harness = hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: org}, func(group *echo.Group, _ hubtest.HTTPDeps) {
			group.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					claims, _ := c.Request().Context().Value(acceptanceClaimsKey{}).(cloudassert.Claims)
					c.Set("claims", claims)
					return next(c)
				}
			})
			group.POST("/internal/v1/invitations/accept", func(c echo.Context) error {
				claims, ok := c.Get("claims").(cloudassert.Claims)
				return s.feature().AcceptHTTP(c, claims, ok, accessinvites.HTTPOptions{Error: hubtest.HTTPError, Invalid: func(c echo.Context, _ error) error { return hubtest.HTTPError(c, hubapi.Invalid("invalid invitation")) }, Refuse: func(c echo.Context, status int, _, _ string, body any) error { return c.JSON(status, body) }})
			})
			group.GET("/organizations/:organization/app/bootstrap", func(c echo.Context) error {
				_, status, err := s.authenticator().HostedCredential(c.Request().Context(), c)
				if err != nil {
					if status >= 500 {
						return hubtest.HTTPError(c, hubauth.Classify(err))
					}
					return c.NoContent(status)
				}
				return c.NoContent(http.StatusOK)
			})
		})
		f.services[string(org)] = s
	}
	return f
}

type acceptanceClaimsKey struct{}

func (f acceptanceFixture) request(t *testing.T, org, path, body string, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
	t.Helper()
	claims := cloudassert.Claims{Kind: cloudassert.KindService, Role: "owner", Audience: org, Subject: org + "_owner", Email: org + "_owner@example.test", Method: http.MethodPost}
	if mutate != nil {
		mutate(&claims)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.WithValue(t.Context(), acceptanceClaimsKey{}, claims), claims.Method, path, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	f.services[org].harness.Echo.ServeHTTP(response, request)
	return response
}
func sharedRouteBrowserClaims(c *cloudassert.Claims) {
	c.Kind = cloudassert.KindBrowser
	c.ProviderOrganization = "provider_" + c.Audience
	c.ProviderSession = "session_" + c.Audience
	c.SessionCreatedAt = time.Now().Add(-time.Minute)
	c.SessionExpiresAt = time.Now().Add(time.Hour)
	c.AccessExpiresAt = time.Now().Add(time.Hour)
	c.Binding = strings.Repeat("a", 64)
	c.CSRF = "example-csrf"
}
func requireEntryAuthResponse(t *testing.T, status int, body string, want int) {
	t.Helper()
	if status != want {
		t.Fatalf("status=%d body=%s want=%d", status, body, want)
	}
	if want == http.StatusServiceUnavailable && strings.Contains(body, "private-outage-sentinel") {
		t.Fatalf("dependency error leaked: %s", body)
	}
}
