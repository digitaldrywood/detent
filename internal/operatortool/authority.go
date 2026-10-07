package operatortool

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

var ErrAccessDenied = errors.New("operator access is unavailable")

// Identity binds a connection to an authenticated credential/session and one
// organization. It contains no grants: permissions are resolved for each call.
type Identity struct {
	PrincipalID    string `json:"principal_id"`
	OrganizationID string `json:"organization_id"`
	CredentialID   string `json:"credential_id"`
	SessionID      string `json:"session_id,omitempty"`
}

func (i Identity) Valid() bool {
	return i.PrincipalID != "" && i.OrganizationID != "" && i.CredentialID != ""
}

// Requirement is application authority, independent of MCP annotations or
// confirmation. Resource ownership must be checked by the application adapter.
type Requirement struct {
	OrganizationWide bool // Administration cannot be performed by a project-scoped credential.
	Scope            apikey.Scope
	OrganizationID   string
	ProjectID        string
	ResourceKind     string
	ResourceID       string
}

// Account is fresh application identity for account commands. It is not a
// credential and must never be retained as permission evidence.
type Account struct {
	Subject      string
	Role         string
	Email        string
	SupportActor string
}

// Authority delegates permission decisions and projection to dashboard services.
type Authority struct {
	Account  Account
	Identity Identity
	Check    func(context.Context, Requirement) error
	Snapshot func(context.Context, telemetry.Snapshot) (telemetry.Snapshot, error)
	// BindContext attaches current application command inputs after authorization.
	// It is resolved anew with Check, never retained as a permission or supplied
	// by a transport. Approval must bind the originating connection's inputs.
	BindContext func(context.Context) context.Context
	Explainer   Explainer
	WorkReads   WorkReader
	Changes     ChangeApplication
}

type Connection struct {
	DashboardURL string
	ID           string
	Client       string
	Identity     Identity
	Resolve      func(context.Context) (Authority, error)
}

type connectionKey struct{}
type authorityKey struct{}

func WithConnection(ctx context.Context, connection Connection) context.Context {
	return context.WithValue(ctx, connectionKey{}, connection)
}

func ConnectionIdentity(ctx context.Context) Identity {
	return CurrentConnection(ctx).Identity
}

// ProjectSnapshot applies the current application read boundary to an already
// acquired snapshot. Internal dashboard/chat reads without this adapter retain
// their existing authorization path.
func ProjectSnapshot(ctx context.Context, snapshot telemetry.Snapshot) (telemetry.Snapshot, error) {
	if authority, ok := ctx.Value(authorityKey{}).(Authority); ok {
		if authority.Snapshot == nil {
			return telemetry.Snapshot{}, ErrAccessDenied
		}
		return authority.Snapshot(ctx, snapshot)
	}
	return snapshot, nil
}

func AuthorizeCurrent(ctx context.Context, requirement Requirement) (context.Context, error) {
	connection, ok := ctx.Value(connectionKey{}).(Connection)
	if !ok || !connection.Identity.Valid() || connection.Resolve == nil {
		return ctx, ErrAccessDenied
	}
	authority, err := connection.Resolve(ctx)
	if err != nil || authority.Identity != connection.Identity || authority.Check == nil {
		return ctx, ErrAccessDenied
	}
	if requirement.OrganizationID == "" {
		requirement.OrganizationID = connection.Identity.OrganizationID
	}
	if requirement.OrganizationID != connection.Identity.OrganizationID || !apikey.ValidScope(requirement.Scope) {
		return ctx, ErrAccessDenied
	}
	if err := authority.Check(ctx, requirement); err != nil {
		if errors.Is(err, ErrAccessDenied) {
			return ctx, err
		}
		return ctx, ErrAccessDenied
	}
	if authority.BindContext != nil {
		ctx = authority.BindContext(ctx)
	}
	return context.WithValue(ctx, authorityKey{}, authority), nil
}

type AuthorizedExecutor struct {
	executor *Executor
}

func NewAuthorizedExecutor(executor *Executor) *AuthorizedExecutor {
	return &AuthorizedExecutor{executor: executor}
}

func (e *AuthorizedExecutor) ListTools(ctx context.Context) ([]Definition, error) {
	authorized, err := AuthorizeCurrent(ctx, Requirement{Scope: apikey.ScopeRead})
	if err != nil {
		return nil, err
	}
	definitions := make([]Definition, 0, len(Catalog()))
	if e.executor == nil {
		return definitions, nil
	}
	authority, present := authorized.Value(authorityKey{}).(Authority)
	if !present {
		authority = Authority{}
	}
	for _, definition := range Catalog() {
		if definition.Name == ExplainItem && (e.executor.explainer != nil || authority.Explainer != nil) || definition.Name != ExplainItem && e.executor.snapshots != nil {
			definitions = append(definitions, definition)
		}
	}

	if e.executor.workReads != nil || authority.WorkReads != nil {
		reader := e.executor.workReads
		if authority.WorkReads != nil {
			reader = authority.WorkReads
		}
		allowed := WorkReadCatalog()
		if available, ok := reader.(interface {
			WorkReadNames(context.Context) []string
		}); ok {
			names := available.WorkReadNames(authorized)
			allowed = slices.DeleteFunc(allowed, func(d Definition) bool { return !slices.Contains(names, d.Name) })
		}
		definitions = append(definitions, allowed...)
	}
	return definitions, nil
}

func (e *AuthorizedExecutor) Execute(ctx context.Context, call Call) (Result, error) {
	if _, ok := Lookup(call.Name); !ok {
		return Result{}, ErrUnknownTool
	}
	if IsWorkRead(call.Name) {
		request, err := DecodeWorkRead(call.Name, call.Arguments)
		if err != nil {
			return Result{}, err
		}
		ctx, err = AuthorizeCurrent(ctx, Requirement{Scope: apikey.ScopeRead, ProjectID: request.ProjectID})
		if err != nil {
			return Result{}, err
		}
		if e.executor == nil {
			return Result{}, ErrReadUnavailable
		}
		return e.executor.readWork(ctx, call)
	}
	// Decode the bounded resource selector before reaching any read service. The
	// tool's own typed decoder still validates which fields its schema permits.
	var selector struct {
		ProjectID string `json:"project_id"`
		Reference string `json:"reference"`
		State     string `json:"state"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArguments(call.Arguments, &selector); err != nil {
		return Result{}, err
	}
	ctx, err := AuthorizeCurrent(ctx, Requirement{Scope: apikey.ScopeRead, ProjectID: strings.TrimSpace(selector.ProjectID)})
	if err != nil {
		return Result{}, err
	}
	if e.executor == nil {
		return Result{}, ErrSnapshotUnavailable
	}
	return e.executor.Execute(ctx, call)
}

// CurrentConnection returns trusted transport binding, never tool arguments.
func CurrentConnection(ctx context.Context) Connection {
	connection, ok := ctx.Value(connectionKey{}).(Connection)
	if !ok {
		return Connection{}
	}
	return connection
}

func BindConnection(ctx context.Context, id, client string) context.Context {
	connection := CurrentConnection(ctx)
	connection.ID, connection.Client = id, client
	return WithConnection(ctx, connection)
}

// CurrentChanges returns the application adapter bound by the latest authority
// resolution. It must never be retained across calls or operator approval.
func CurrentChanges(ctx context.Context) (ChangeApplication, error) {
	authority, ok := ctx.Value(authorityKey{}).(Authority)
	if !ok || authority.Changes == nil {
		return nil, ErrServiceUnavailable
	}
	return authority.Changes, nil
}
