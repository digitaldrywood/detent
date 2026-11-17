package hubauth

import (
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Identity(credential Credential, organization string) operatortool.Identity {
	principal := credential.ID
	if credential.HostedPrincipal != "" {
		principal = credential.HostedPrincipal
	}
	return operatortool.Identity{PrincipalID: principal, OrganizationID: organization, CredentialID: credential.Hash, SessionID: credential.SessionHash}
}

func ProjectFilter(credential Credential, project tracker.ProjectID, after tracker.ProjectID, limit int) accessstore.ProjectAccessFilter {
	filter := accessstore.ProjectAccessFilter{
		UserID:        hostedSubject(credential),
		Role:          credential.HostedRole,
		PrincipalID:   credential.HostedPrincipal,
		CredentialID:  credential.ID,
		KeyKind:       credential.HostedKeyKind,
		ProjectAccess: credential.HostedProjectAccess,
		After:         after,
		Only:          project,
		Limit:         limit,
	}
	if credential.EntryKey != nil {
		filter.KeyKind = credential.EntryKey.Kind
		filter.ProjectAccess = credential.EntryKey.Access
		filter.Projects = make([]tracker.ProjectID, len(credential.EntryKey.Projects))
		for i, id := range credential.EntryKey.Projects {
			filter.Projects[i] = tracker.ProjectID(id)
		}
		filter.BlockedProjects = make([]tracker.ProjectID, len(credential.EntryKey.BlockedProjects))
		for i, id := range credential.EntryKey.BlockedProjects {
			filter.BlockedProjects[i] = tracker.ProjectID(id)
		}
		filter.RequireCredentialGrant = false
	} else {
		filter.RequireCredentialGrant = credential.HostedProjectAccess == "selected"
	}
	return filter
}

func KeyAllows(scope, required apikey.Scope) bool {
	return apikey.ValidScope(scope) && apikey.HasScope([]string{string(scope)}, required)
}

func RoleAllows(role string, scope apikey.Scope, action ...string) bool {
	return accessstore.RoleAllows(role, scope, action...)
}

func ReadRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodGet || c.Request().Method == http.MethodHead
}

func RunnerOperationAllowed(c echo.Context, operations []string) bool {
	path := c.Path()
	if (path == runnerBase+"/:runner" || path == runnerBase+"/:runner/routing") && c.Request().Method == http.MethodGet || path == runnerBase+"/:runner/renew" || path == runnerBase+"/:runner/rotate" {
		return true
	}
	operation := ""
	switch {
	case path == runnerBase+"/:runner/update/receipt":
		operation = runnerauth.Heartbeat
	case path == "/api/v2/capabilities":
		operation = runnerauth.Read
	case !strings.HasPrefix(path, nativeBase):
		return false
	case path == nativeBase+"/claims", path == nativeBase+"/claims/preview", path == nativeBase+"/claims/wait", path == nativeBase+"/leases/:lease/renew", path == nativeBase+"/leases/:lease/release", path == nativeBase+"/leases/:lease/validate":
		operation = runnerauth.Claim
	case c.Request().Method == http.MethodGet:
		operation = runnerauth.Read
	case path == nativeBase+"/runner-sessions/find":
		operation = runnerauth.Read
	case path == nativeBase+"/runner-sessions", path == nativeBase+"/security-audits/dispositions":
		operation = runnerauth.Events
	case path == nativeBase+"/attempts/:attempt/diff", path == nativeBase+"/attempts/:attempt/diff/check":
		operation = runnerauth.Claim
	case path == nativeBase+"/onboarding/issue-intake/result", path == nativeBase+"/onboarding/repositories/report":
		operation = runnerauth.Heartbeat
	case path == nativeBase+"/work-items/:item/source-intake":
		operation = runnerauth.Claim
	case path == nativeBase+"/machines/register", path == nativeBase+"/machines/:machine/heartbeat", path == nativeBase+"/policy/observed":
		operation = runnerauth.Heartbeat
	case strings.HasPrefix(path, nativeBase+"/workspaces/:workspace/worker/"):
		operation = runnerauth.Claim
	case path == nativeBase+"/work-items/:item/events", path == nativeBase+"/conversations/:conversation/turn-events" && c.Request().Method == http.MethodPost:
		operation = runnerauth.Events
	case path == nativeBase+"/attempts/:attempt/evidence", path == nativeBase+"/attempts/:attempt/evidence/check":
		operation = runnerauth.Events
	case path == nativeBase+"/go-test-receipts", path == nativeBase+"/go-test-receipts/attestations":
		operation = runnerauth.Events
	case strings.HasPrefix(path, attachmentMetadataBase):
		operation = runnerauth.Collaborate
	case strings.HasPrefix(path, nativeBase+"/work-items"):
		operation = runnerauth.Collaborate
	}
	return operation != "" && slices.Contains(operations, operation)
}

func hostedSubject(credential Credential) string {
	if credential.Hosted != nil {
		return credential.Hosted.Subject
	}
	return ""
}

func ReporterOwns(input Input, actor tracker.Actor) bool {
	principal := Actor(input.Credential, input.Organization).PrincipalID
	return input.Credential.HostedRole != "reporter" || principal != "" && actor.PrincipalID == principal
}
