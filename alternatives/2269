package hubfixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessmembers"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	HostedCookie      = "detent_hosted_session"
	hostedSessionTTL  = 30 * 24 * time.Hour
	hostedCSRFContext = "detent-hosted-csrf:"
)

func HostedCSRF(token string) string {
	digest := sha256.Sum256([]byte(hostedCSRFContext + token))
	return hex.EncodeToString(digest[:])
}

type Hosted struct {
	*Hub
	Provider *HostedProvider
	Project  tracker.ProjectID
	Base     string
}

type HostedUser struct {
	Identity auth.Identity
	Token    string
}

func HostedSecurityConfig(provider *HostedProvider) Config {
	plans := hostedplans.PilotPlans()
	return Config{Hosted: &hostedplans.HostedConfig{
		Plans:                &plans,
		OrganizationID:       "org_security",
		WorkOSOrganizationID: "org_provider",
		BootstrapSubject:     "user_owner",
		PublicURL:            "http://127.0.0.1:7777",
		StaffEmails:          []string{"staff@example.test", "support@example.test"},
		SupportActors:        []string{"support@example.test"},
		Provider:             provider,
	}}
}

func OpenHosted(t *testing.T, cfg Config) *Hosted {
	t.Helper()
	if cfg.Hosted == nil {
		t.Fatal("hubfixture.OpenHosted needs a hosted configuration")
	}
	var provider *HostedProvider
	if hostedProvider, ok := cfg.Hosted.Provider.(*HostedProvider); ok {
		provider = hostedProvider
	}
	if cfg.Hosted.Provider == nil {
		provider = NewHostedProvider()
		hosted := *cfg.Hosted
		hosted.Provider = provider
		cfg.Hosted = &hosted
	}
	cfg = defaults(t, cfg)
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithClock(cfg.Clock))
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := cfg.Clock().UTC().Truncate(time.Microsecond)
	deployment, generation := cfg.Hosted.Deployment()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: "local-organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.AddAccessOrganizationSlug(t.Context(), "local-organization"); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: deployment, PublicURL: cfg.Hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	SeedAdmin(t, repository, organization, string(cfg.InitialAdminToken), now)
	h := open(t, &Hub{Admin: admin, Primary: organization, owner: t}, cfg, repository)
	return &Hosted{Hub: h, Provider: provider}
}

func OpenHostedSecurity(t *testing.T, configure ...func(*Config)) (*Hosted, tracker.ProjectID, string) {
	t.Helper()
	cfg := HostedSecurityConfig(NewHostedProvider())
	for _, apply := range configure {
		apply(&cfg)
	}
	h := OpenHosted(t, cfg)
	project := tracker.ProjectID("prj_security")
	h.SeedProject(t, project, "private-project-sentinel")
	h.Project, h.Base = project, ProjectBase(h.Primary, project)
	return h, project, h.Base
}

func NewHostedSecurity(t *testing.T, configure ...func(*Config)) *Hosted {
	t.Helper()
	h, _, _ := OpenHostedSecurity(t, configure...)
	return h
}

func (h *Hosted) SeedProject(t *testing.T, id tracker.ProjectID, name string) tracker.NativeProject {
	t.Helper()
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	project := hubtest.Project(t, h.Repository, h.Primary, hubtest.ProjectOptions{ID: id, Name: name, States: states})
	hubtest.Policy(t, h.Repository, h.Config.RunDetails, project, hubtest.PolicyOptions{Now: h.Now()})
	return project
}

func (h *Hosted) Identity(subject, email, providerOrganization, supportActor string) auth.Identity {
	now := time.Now().UTC()
	identity := auth.Identity{Subject: subject, Email: email, EmailVerified: true, Hosted: &auth.HostedIdentity{
		Subject: subject, OrganizationID: providerOrganization, SessionID: "session_" + strings.TrimPrefix(subject, "user_") + "_" + strings.TrimPrefix(hubapi.NewID("s"), "s_"),
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), SupportActor: supportActor,
	}}
	if supportActor != "" {
		identity.Hosted.SupportReason = "customer-request"
	}
	return identity
}

func (h *Hosted) AddMember(t *testing.T, identity auth.Identity, membership auth.Membership) {
	t.Helper()
	if err := accessmembers.New(accessmembers.Deps{Repository: h.Repository, Organization: h.Primary, Now: h.Config.Clock, GenerateToken: apikey.GenerateToken}).Publish(t.Context(), identity, membership, ""); err != nil {
		t.Fatal(err)
	}
}

func (h *Hosted) Session(t *testing.T, identity auth.Identity) string {
	t.Helper()
	if h.Provider != nil {
		h.Provider.AddSession(*identity.Hosted)
	}
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	now := h.Now().UTC()
	expires := now.Add(hostedSessionTTL)
	if identity.Hosted.ExpiresAt.Before(expires) {
		expires = identity.Hosted.ExpiresAt
	}
	hosted := identity.Hosted
	if err := h.Repository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: h.Primary, Hash: apikey.HashToken(token), Email: strings.ToLower(strings.TrimSpace(identity.Email)), Subject: hosted.Subject, ProviderOrganizationID: hosted.OrganizationID, SessionID: hosted.SessionID, IdentityCreatedAt: hosted.CreatedAt, IdentityExpiresAt: hosted.ExpiresAt, SupportActor: hosted.SupportActor, SupportReason: hosted.SupportReason, CreatedAt: now, ExpiresAt: expires})
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func (h *Hosted) User(t *testing.T, name, role, email, grant, supportActor string) HostedUser {
	t.Helper()
	identity := h.Identity("user_"+name, email, h.Config.Hosted.WorkOSOrganizationID, supportActor)
	identity.Hosted.SessionID = "session_" + name
	membership, err := h.Provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, role)
	if err != nil {
		t.Fatal(err)
	}
	h.AddMember(t, identity, membership)
	user := HostedUser{Identity: identity, Token: h.Session(t, identity)}
	if grant != "" {
		h.Grant(t, user, h.Project, grant == "write", false)
	}
	return user
}

func (h *Hosted) Grant(t *testing.T, user HostedUser, project tracker.ProjectID, write, runner bool) {
	t.Helper()
	if err := h.Repository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.Identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: h.Primary, UserID: user.Identity.Subject, ProjectID: project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: h.Primary, TokenID: member.PrincipalID, ProjectID: project})
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *Hosted) Revoke(t *testing.T, user HostedUser, project tracker.ProjectID) {
	t.Helper()
	if err := h.Repository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.Identity.Subject)
		if err != nil {
			return err
		}
		if err := w.DeleteAccessProjectGrant(t.Context(), user.Identity.Subject, project); err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, project); err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *Hosted) Request(t *testing.T, user HostedUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	contentType := ""
	switch value := body.(type) {
	case nil:
	case url.Values:
		reader = strings.NewReader(value.Encode())
		contentType = "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
		contentType = "application/json"
	}
	request := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	if user.Token != "" {
		request.AddCookie(&http.Cookie{Name: HostedCookie, Value: user.Token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		request.Header.Set("X-CSRF-Token", HostedCSRF(user.Token))
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	return response
}

func (h *Hosted) Page(t *testing.T, user HostedUser, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	if user.Token != "" {
		request.AddCookie(&http.Cookie{Name: HostedCookie, Value: user.Token, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	}
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	return response
}

func SeedHostedSecurityProject(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID) error {
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_security", Name: "private-project-sentinel", Profile: "native", CreatedAt: time.Now().UTC()}, States: states})
		return err
	})
}
