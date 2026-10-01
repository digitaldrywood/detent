package operatortool

import (
	"context"
	"errors"
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
	Scope          apikey.Scope
	OrganizationID string
	ProjectID      string
	ResourceKind   string
	ResourceID     string
}

// Authority delegates permission decisions and read projection to the same
// application services used by the dashboard. Neither transport defines roles.
type Authority struct {
	Identity Identity
	Check    func(context.Context, Requirement) error
	Snapshot func(context.Context, telemetry.Snapshot) (telemetry.Snapshot, error)
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
	connection, _ := ctx.Value(connectionKey{}).(Connection)
	return connection.Identity
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

// AuthorizeCurrent must also be called when executing an approved action, using
// its original connection identity and a freshly resolved authority. Approval,
// discovery and YOLO never grant access; callers cannot cache this context.
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
		return ctx, ErrAccessDenied
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
	if _, err := AuthorizeCurrent(ctx, Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	definitions := make([]Definition, 0, len(Catalog()))
	if e.executor == nil {
		return definitions, nil
	}
	for _, definition := range Catalog() {
		if definition.Name == ExplainItem && e.executor.explainer != nil || definition.Name != ExplainItem && e.executor.snapshots != nil {
			definitions = append(definitions, definition)
		}
	}
	return definitions, nil
}

func (e *AuthorizedExecutor) Execute(ctx context.Context, call Call) (Result, error) {
	if _, ok := Lookup(call.Name); !ok {
		return Result{}, ErrUnknownTool
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
	connection, _ := ctx.Value(connectionKey{}).(Connection)
	return connection
}

// BindConnection identifies the authenticated protocol connection; it does not
// select confirmation mode or grant authority.
func BindConnection(ctx context.Context, id, client string) context.Context {
	connection := CurrentConnection(ctx)
	connection.ID, connection.Client = id, client
	return WithConnection(ctx, connection)
}
