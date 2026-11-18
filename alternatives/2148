package hubauth

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type Response struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Resolver func(context.Context) (Credential, error)

type OperatorOptions struct {
	Classify          func(error) error
	Error             func(echo.Context, error) error
	Refuse            func(echo.Context, int, string, string, any) error
	AccountCredential func(context.Context, string) (Credential, error)
	EntryOrigin       func(context.Context, string) (Credential, error)
	CurrentSession    func(context.Context, string, time.Time) (auth.Session, error)
	AccessTime        func(context.Context, time.Time) time.Time
	Authority         func(context.Context, Credential, string) (operatortool.Authority, error)
	Bind              func(context.Context, string, string, Credential, Resolver) context.Context
	Catalog           func(context.Context, Credential, string) context.Context
	DashboardURL      string
	PublicURL         string
	ResolveEntryKey   bool
}

type AuthorityOptions struct {
	Classify             func(error) error
	Base                 func(Input) operatortool.Authority
	KeyCredential        func(context.Context, Credential) (Credential, error)
	RunnerAdministration func(context.Context, Credential) error
	RecheckMutation      func(context.Context, Input) error
	Resource             func(context.Context, Input, operatortool.Requirement) error
}

type resolverKey struct{}

func WithResolver(ctx context.Context, resolve func(context.Context) (Input, error)) context.Context {
	return context.WithValue(ctx, resolverKey{}, resolve)
}
func Resolve(ctx context.Context) (Input, error) {
	resolve, ok := ctx.Value(resolverKey{}).(func(context.Context) (Input, error))
	if !ok {
		return Input{}, operatortool.ErrAccessDenied
	}
	return resolve(ctx)
}
func (s *Authenticator) shared() bool { return s.deps.Hosted != nil && s.deps.Hosted.Shared }

func (s *Authenticator) AuthorizeProject(ctx context.Context, input Input, write bool) error {
	if s.deps.AccessRepository == nil {
		return hubapi.NotFound()
	}
	return s.read(ctx, input.Organization, func(r tenantstore.Reader) error { return s.Authorize(ctx, r, input, write) })
}
func (s *Authenticator) ReadProject(ctx context.Context, input Input) (projectstore.Record, error) {
	var result projectstore.Record
	err := s.deps.Repository.ReadTenant(ctx, input.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result, err = r.ProjectRecord(ctx, input.Project)
		return err
	})
	return result, RecordError(err)
}
func RecordError(err error) error {
	if errors.Is(err, corestore.ErrNotFound) {
		return hubapi.NotFound()
	}
	return err
}

func Resource(ctx context.Context, input Input, requirement operatortool.Requirement) error {
	if requirement.ResourceKind == "" && requirement.ResourceID == "" {
		return nil
	}
	return operatortool.ErrAccessDenied
}

func (s *Authenticator) DefaultAuthorityOptions() AuthorityOptions {
	return AuthorityOptions{
		Classify:             Classify,
		Base:                 func(Input) operatortool.Authority { return operatortool.Authority{} },
		KeyCredential:        func(context.Context, Credential) (Credential, error) { return Credential{}, accessstore.ErrDenied },
		RunnerAdministration: func(context.Context, Credential) error { return accessstore.ErrDenied },
		RecheckMutation:      func(ctx context.Context, input Input) error { return s.AuthorizeProject(ctx, input, true) },
		Resource:             Resource,
	}
}

type NativeOptions struct {
	Credential          func(echo.Context) (Credential, bool)
	EventScope          func(echo.Context, Input) bool
	ArtifactRead        func(echo.Context) bool
	RelayTicket         func(echo.Context) bool
	LeaseRoute          func(string) bool
	RepositoryRoute     func(string) bool
	AuthorizeLease      func(context.Context, Input) error
	AuthorizeRepository func(context.Context, Input) error
	ReadProject         func(context.Context, Input, bool, bool) error
	ReadItem            func(context.Context, Input, string) error
	RecordError         func(error) error
	Store               func(echo.Context, Input)
}

type APIOptions struct {
	Classify              func(error) error
	Error                 func(echo.Context, error) error
	Refuse                func(echo.Context, int, string, string, any) error
	IsNotFound            func(error) bool
	HostedAdministration  echo.MiddlewareFunc
	ArtifactPublisher     func(echo.Context, Credential) bool
	ChangePrincipal       func(echo.Context, Credential) bool
	ConfigurationRank     func(echo.Context) bool
	CompatibilityResource func(echo.Context) error
	AdmitRunner           func(echo.Context, Credential) error
}
