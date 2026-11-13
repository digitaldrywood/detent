package hubscope

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorchat"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerenroll"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func HostedReadRequest(c echo.Context) bool {
	return hubauth.ReadRequest(c)
}

func HostedKeyAllows(scope, required apikey.Scope) bool {
	return hubauth.KeyAllows(scope, required)
}

func HostedRoleAllows(role string, scope apikey.Scope, action ...string) bool {
	return hubauth.RoleAllows(role, scope, action...)
}

func AuthCredential(c hubapi.APICredential) hubauth.Credential {
	return hubauth.Credential{EntryKey: c.EntryKey, Hosted: c.Hosted, HostedRole: c.HostedRole, HostedMembership: c.HostedMembership, HostedPrincipal: c.HostedPrincipal, HostedKeyScope: c.HostedKeyScope, HostedKeyKind: c.HostedKeyKind, HostedProjectAccess: string(c.HostedProjectAccess), ManageRunners: c.ManageRunners, SessionHash: c.SessionHash, Hash: c.Hash, ID: c.ID, Name: c.Name, Scope: hubauth.TokenScope(c.Scope), NativeOnly: c.NativeOnly, Runner: c.Runner, Agent: c.Agent, RunnerRenewal: c.RunnerRenewal, RequestCapacity: c.RequestCapacity}
}

func NativeCredential(c hubauth.Credential) hubapi.APICredential {
	return hubapi.APICredential{EntryKey: c.EntryKey, Hosted: c.Hosted, HostedRole: c.HostedRole, HostedMembership: c.HostedMembership, HostedPrincipal: c.HostedPrincipal, HostedKeyScope: c.HostedKeyScope, HostedKeyKind: c.HostedKeyKind, HostedProjectAccess: hubapi.HostedProjectAccess(c.HostedProjectAccess), ManageRunners: c.ManageRunners, SessionHash: c.SessionHash, Hash: c.Hash, ID: c.ID, Name: c.Name, Scope: hubapi.APIScope(c.Scope), NativeOnly: c.NativeOnly, Runner: c.Runner, Agent: c.Agent, RunnerRenewal: c.RunnerRenewal, RequestCapacity: c.RequestCapacity}
}

func NativeCredentialResult(c hubauth.Credential, status int, err error) (hubapi.APICredential, int, error) {
	return NativeCredential(c), status, hubapi.FromFeatureError(err)
}

func NativeIssuePrincipal(alias string) string {
	return "(SELECT actor_principal_id FROM native_issue_actors WHERE id=" + alias + ".id)"
}

type GitHubIntakeRevision struct {
	RequireAdminKey bool                  `json:"-"`
	Issuer          *OperatorChatIdentity `json:"-"`
	Authority       *NativeScope          `json:"-"`
	Reporter        *NativeScope          `json:"-"`
	FallbackRunner  string                `json:"-"`
	BatchID         string                `json:"-"`
	ImportID        string                `json:"-"`
	Revision        int64                 `json:"-"`
}

type NativeScope struct {
	IntegrationOwned      bool                  `json:"-"`
	GitHubIntakeRevision  *GitHubIntakeRevision `json:"-"`
	IntegrationRepository string                `json:"-"`
	duplicateSearch       *NativeDuplicateSearch
	Action                string                 `json:"-"`
	ActionItem            string                 `json:"-"`
	ActionTarget          string                 `json:"-"`
	ActionComment         string                 `json:"-"`
	ScheduledRepair       *NativeScheduledRepair `json:"-"`
	SourceActor           *tracker.Actor         `json:"-"`
	ProgressOrigin        ProgressDefectOrigin   `json:"-"`
	Organization          tracker.OrganizationID `json:"-"`
	Project               tracker.ProjectID      `json:"-"`
	Credential            hubapi.APICredential   `json:"-"`
	RequireHostedAdmin    bool                   `json:"-"`
	ImportedContent       bool                   `json:"-"`
	ManagedRunner         string                 `json:"-"`
	managedRunnerScope    string
	managedRunnerProjects []tracker.ProjectID
}

type ProgressDefectOrigin interface {
	AuthorizeProgressDefect(ctx context.Context, w tenantstore.Writer, owner any, scope NativeScope, leaseID tracker.LeaseID, fencingToken tracker.FencingToken) error
}

func NativeRequestScope(c echo.Context) NativeScope {
	scope, ok := c.Get("native_scope").(NativeScope)
	if !ok {
		return NativeScope{}
	}
	return scope
}

func (scope NativeScope) Actor() tracker.Actor {
	if scope.SourceActor != nil {
		return *scope.SourceActor
	}
	kind := "human"
	if scope.Credential.Scope == hubapi.APIScopeWorker || scope.Credential.Runner.RunnerID != "" {
		kind = "runner"
	} else if scope.Credential.EntryKey != nil && scope.Credential.EntryKey.Kind == "service" || scope.Credential.HostedKeyScope != "" && scope.Credential.HostedKeyKind == "service" {
		kind = "integration"
	}
	principal := OperatorIdentity(scope.Credential, string(scope.Organization)).PrincipalID
	if kind == "integration" && scope.Credential.HostedKeyKind == "service" {
		principal = scope.Credential.ID
	}
	return tracker.Actor{Kind: kind, PrincipalID: principal, ViaKey: scope.Credential.ViaKey(), Agent: scope.Credential.Agent}
}

func (scope NativeScope) ReporterOwns(actor tracker.Actor) bool {
	return scope.Credential.HostedRole != "reporter" || scope.Actor().PrincipalID != "" && actor.PrincipalID == scope.Actor().PrincipalID
}

func (scope NativeScope) IssueVisibilitySQL(alias string) (string, []any) {
	if scope.Credential.HostedRole != "reporter" {
		return "1", nil
	}
	return "COALESCE(" + NativeIssuePrincipal(alias) + ", '') = ? AND ? <> ''", []any{scope.Actor().PrincipalID, scope.Actor().PrincipalID}
}

func ReporterStatus(state string) string {
	switch state {
	case "Triage":
		return "submitted"
	case "Backlog", "Todo":
		return "planned"
	case "In Progress", "In Review", "Rework", "Merging", "Blocked":
		return "in_progress"
	case "Done":
		return "done"
	case "Cancelled":
		return "cancelled"
	default:
		return ""
	}
}

func (scope NativeScope) ReporterIssue(issue tracker.NativeIssue) tracker.NativeIssue {
	issue.ReporterStatus = ""
	if scope.Credential.HostedRole == "reporter" {
		issue.ReporterStatus = ReporterStatus(issue.State)
	}
	return issue
}

type OperatorEntryOriginKey struct{}

type OperatorAuthorizationFailure struct {
	Cause error `json:"-"`
	kind  error
}

func (e *OperatorAuthorizationFailure) Error() string { return e.kind.Error() }

func (e *OperatorAuthorizationFailure) Unwrap() error { return e.kind }

func (e *OperatorAuthorizationFailure) Is(target error) bool {
	return errors.Is(e.kind, target) || target == tenantauth.ErrUnavailable && (errors.Is(e.kind, operatortool.ErrServiceUnavailable) || errors.Is(e.kind, operatortool.ErrProviderUnavailable))
}

func OperatorAuthorizationError(err error) error {
	var failure *OperatorAuthorizationFailure
	if errors.As(err, &failure) {
		return failure
	}
	var native *hubapi.NativeError
	if errors.As(err, &native) {
		if native.Status == http.StatusForbidden || native.Status == http.StatusNotFound {
			return operatortool.ErrAccessDenied
		}
	}
	var denied *operatortool.AuthorizationError
	if errors.As(err, &denied) && errors.As(denied.Cause, &native) {
		return OperatorAuthorizationError(denied.Cause)
	}
	classified := hubauth.Classify(err)
	var moved *hubauth.AuthorizationFailure
	if errors.As(classified, &moved) {
		return &OperatorAuthorizationFailure{Cause: moved.Cause, kind: moved.Kind}
	}
	return classified
}

func OperatorIdentity(credential hubapi.APICredential, organization string) operatortool.Identity {
	return hubauth.Identity(AuthCredential(credential), organization)
}

type OperatorChatIdentity = operatorchat.Identity

type NativeOperatorScopeKey struct{}

func CanManageProjectSecrets(credential hubapi.APICredential) bool {
	if credential.Runner.RunnerID != "" {
		return false
	}
	if credential.Hosted != nil {
		return credential.HostedRole == "owner" || credential.HostedRole == "admin"
	}
	return credential.Scope == hubapi.APIScopeAdmin && !credential.NativeOnly
}

func RunnerUnauthorized() error { return hubapi.FromFeatureError(runnerenroll.Unauthorized()) }
