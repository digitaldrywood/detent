package hubauth

import (
	"context"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type TokenScope string

const (
	Worker                 TokenScope = "worker"
	Operator               TokenScope = "operator"
	Admin                  TokenScope = "admin"
	maxBearerTokenBytes               = 4096
	NativeBase                        = "/api/v2/organizations/:organization/projects/:project"
	nativeBase                        = NativeBase
	runnerBase                        = "/api/v2/organizations/:organization/runners"
	attachmentMetadataBase            = nativeBase + "/attachment-metadata"
	Cookie                            = "detent_hosted_session"
	workBoardPath                     = "/api/v2/organizations/:organization/work-board"
)

type Credential struct {
	EntryKey            *apikey.KeyAuthority
	Hosted              *auth.HostedIdentity
	HostedRole          string
	HostedMembership    string
	HostedPrincipal     string
	HostedKeyScope      apikey.Scope
	HostedKeyKind       string
	HostedProjectAccess string
	ManageRunners       bool
	SessionHash         string
	Hash                string
	ID                  string
	Name                string
	Scope               TokenScope
	NativeOnly          bool
	Runner              runnerauth.Identity
	Agent               string
	RunnerRenewal       bool
	RequestCapacity     *int
}

type Hosted struct {
	OrganizationID string
	Shared         bool
	StaffEmails    []string
	SupportActors  []string
}

type Deps struct {
	TrustedProxy       bool
	GenerateToken      func() (string, error)
	Classify           func(error) error
	Memberships        func(context.Context, string, string) ([]auth.Membership, error)
	CurrentSession     func(context.Context, auth.HostedIdentity) (auth.HostedIdentity, error)
	SessionLookup      func(context.Context, string) (auth.Session, error)
	SharedSession      func(context.Context, echo.Context) (auth.Session, string, error)
	Repository         tenantstore.Repository
	AccessRepository   tenantstore.Repository
	Organization       tracker.OrganizationID
	Now                func() time.Time
	Hosted             *Hosted
	CompleteInvitation func(context.Context, auth.Session) (bool, error)
	BoardCredential    func(context.Context) (Credential, bool)
	SharedClaims       func(echo.Context) (cloudassert.Claims, bool)
	RunnerEventRoute   func(echo.Context) bool
	RunnerEventContext func(context.Context) context.Context
	StoreCredential    func(echo.Context, Credential)
}

type Authenticator struct{ deps Deps }

func New(deps Deps) *Authenticator {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.BoardCredential == nil {
		deps.BoardCredential = func(context.Context) (Credential, bool) { return Credential{}, false }
	}
	if deps.SharedClaims == nil {
		deps.SharedClaims = func(echo.Context) (cloudassert.Claims, bool) { return cloudassert.Claims{}, false }
	}
	if deps.StoreCredential == nil {
		deps.StoreCredential = func(c echo.Context, credential Credential) { c.Set("hub_auth_credential", credential) }
	}
	if deps.CompleteInvitation == nil {
		deps.CompleteInvitation = func(context.Context, auth.Session) (bool, error) { return false, nil }
	}

	if deps.Classify == nil {
		deps.Classify = Classify
	}
	s := &Authenticator{deps: deps}
	return s
}

func (s *Authenticator) read(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if s.deps.AccessRepository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.AccessRepository.ReadTenant(ctx, organization, corestore.Serving, read)
}

type runnerAuthorityReader interface {
	ReadRunnerAuthority(context.Context, tenantauth.RequestIdentity, func() time.Time) (tenantauth.Scope, int, error)
}

func emailListed(emails []string, value string) bool {
	for _, email := range emails {
		if strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

type Input struct {
	Organization          tracker.OrganizationID
	Project               tracker.ProjectID
	Credential            Credential
	Admin                 bool
	Action                string
	IntegrationRepository string
	IntegrationOwned      bool
	IntegrationFence      func(context.Context, accessstore.Reader) error
}

func (s *Authenticator) Scope(input Input) hubapi.Scope {
	return hubapi.NewProjectScope(hubapi.Facts{Organization: input.Organization, Project: input.Project, Actor: Actor(input.Credential, input.Organization), CredentialID: input.Credential.ID, HostedRole: input.Credential.HostedRole, Admin: input.Admin, Runner: input.Credential.Runner}, func(ctx context.Context, reader accessstore.Reader, project tracker.ProjectID, write bool) error {
		bound := input
		bound.Project = project
		return s.Authorize(ctx, reader, bound, write)
	})
}

func Actor(credential Credential, organization tracker.OrganizationID) tracker.Actor {
	kind := "human"
	if credential.Scope == Worker || credential.Runner.RunnerID != "" {
		kind = "runner"
	} else if credential.EntryKey != nil && credential.EntryKey.Kind == "service" || credential.HostedKeyScope != "" && credential.HostedKeyKind == "service" {
		kind = "integration"
	}
	principal := Identity(credential, string(organization)).PrincipalID
	if kind == "integration" && credential.HostedKeyKind == "service" {
		principal = credential.ID
	}
	via := ""
	if credential.EntryKey != nil {
		via = credential.EntryKey.ID
	} else if credential.HostedKeyScope != "" {
		via = credential.ID
	}
	return tracker.Actor{Kind: kind, PrincipalID: principal, ViaKey: via, Agent: credential.Agent}
}

func RequestCredential(c echo.Context) (Credential, bool) {
	credential, ok := c.Get("hub_auth_credential").(Credential)
	return credential, ok
}

func (s *Authenticator) write(ctx context.Context, organization tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	if s.deps.AccessRepository == nil {
		return hubapi.WorkItemStorageUnavailable()
	}
	return s.deps.AccessRepository.WriteTenant(ctx, organization, corestore.Serving, write)
}
func fingerprint(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
