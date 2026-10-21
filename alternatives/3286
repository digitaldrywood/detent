package web

import (
	"context"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const selfHostedOrganization = "self-hosted"

func (s *Server) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, ok := apiCredentialFromContext(c.Request().Context())
		if !ok {
			return writeAPIAuthError(c, 403, "access_denied", operatortool.ErrAccessDenied.Error())
		}
		identity := operatortool.Identity{PrincipalID: credential.ID, OrganizationID: selfHostedOrganization, CredentialID: credential.ID}
		// Capture authentication inputs, never the pooled Echo context. Resolve
		// uses the same credential service again at execution, after discovery.
		tokens := requestAPITokens(c.Request())
		var token string
		if len(tokens) > 0 {
			token = tokens[0]
			identity.CredentialID = apikey.HashToken(token)
		}
		var sessionToken string
		if _, ok := webSessionFromContext(c.Request().Context()); ok {
			if cookie, err := c.Cookie(webSessionCookieName); err == nil {
				sessionToken = cookie.Value
				identity.SessionID = apikey.HashToken(sessionToken)
			}
		}
		var privateSession string
		if cookie, err := c.Cookie(privateDashboardCookieName); err == nil && s.privateDashboardSessionAuthorized(cookie.Value) {
			privateSession = cookie.Value
			if token == "" && sessionToken == "" {
				identity.SessionID = apikey.HashToken(privateSession)
			}
		}
		organization := strings.TrimSpace(c.Request().Header.Get("X-Detent-Organization"))
		if organization != "" && organization != selfHostedOrganization {
			return writeAPIAuthError(c, 403, "access_denied", operatortool.ErrAccessDenied.Error())
		}
		publicURL := s.mcpPublicURL
		if publicURL == "" {
			scheme := "http"
			if c.Request().TLS != nil {
				scheme = "https"
			}
			publicURL = scheme + "://" + c.Request().Host
		}
		connection := operatortool.Connection{DashboardURL: publicURL, Identity: identity, Resolve: func(ctx context.Context) (operatortool.Authority, error) {
			current := credential
			switch {
			case token != "":
				var err error
				current, err = s.authenticateCandidate(ctx, token, s.apiToken())
				if err != nil || current.ID != identity.PrincipalID {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
			case sessionToken != "":
				if s.sessions == nil {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				if _, err := s.sessions.Authenticate(ctx, sessionToken); err != nil {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
			case privateSession != "":
				if !s.privateDashboardSessionAuthorized(privateSession) {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				if !s.dashboardAccess().AllowWrite {
					current.Scopes = []string{string(apikey.ScopeRead)}
				}
			default:
				// The daemon's existing loopback read authorization is sufficient
				// for legacy stdio; the client never invents this authority.
				if !serverAddressLoopback(s.serverAddr) || s.apiToken() != "" {
					return operatortool.Authority{}, operatortool.ErrAccessDenied
				}
				current.Scopes = []string{string(apikey.ScopeRead)}
			}
			return operatortool.Authority{
				Identity: identity,
				Changes:  dashboardChangeApplication{server: s, token: token},
				Check: func(ctx context.Context, requirement operatortool.Requirement) error {
					if requirement.ResourceKind == "runners" && s.hasLocalSettings(ctx) {
						requirement.ResourceKind = "instance"
					}
					if requirement.OrganizationWide && len(current.ProjectIDs) != 0 || !apikey.HasScope(current.Scopes, requirement.Scope) || !apikey.AllowsProject(current.ProjectIDs, requirement.ProjectID) || requirement.ResourceID != "" || requirement.ResourceKind != "" && requirement.ResourceKind != "instance" && requirement.ResourceKind != "credentials" || requirement.ResourceKind == "instance" && len(current.ProjectIDs) != 0 {
						return operatortool.ErrAccessDenied
					}
					return nil
				},
				Snapshot: func(ctx context.Context, snapshot telemetry.Snapshot) (telemetry.Snapshot, error) {
					return operatorScopedSnapshot(snapshot, current.ProjectIDs), nil
				},
			}, nil
		}}
		ctx := operatortool.WithConnection(c.Request().Context(), connection)
		if id := c.Request().Header.Get(operatorConnectionHeader); id != "" {
			if !strings.HasPrefix(id, "stdio-") || len(id) != 54 {
				return writeAPIAuthError(c, 403, "access_denied", operatortool.ErrAccessDenied.Error())
			}
			conversation := s.chat.Conversation(id)
			if conversation.ConnectionID != id {
				return writeAPIAuthError(c, 403, "access_denied", operatortool.ErrAccessDenied.Error())
			}
			ctx = operatortool.BindConnection(ctx, id, "local MCP stdio")
			if err := s.chat.CheckConnection(ctx); err != nil {
				return writeAPIAuthError(c, 403, "access_denied", operatortool.ErrAccessDenied.Error())
			}
		}
		c.SetRequest(c.Request().WithContext(ctx))
		return next(c)
	}
}

// operatorScopedSnapshot reuses the dashboard's project read projection, then
// copies only catalog fields that are attributable to the authorized projects.
// Global logs, budgets and infrastructure details have no project attribution.
func operatorScopedSnapshot(snapshot telemetry.Snapshot, projects []string) telemetry.Snapshot {
	if len(projects) == 0 {
		return snapshot
	}
	out := telemetry.Snapshot{GeneratedAt: snapshot.GeneratedAt, LastKnown: snapshot.LastKnown, LastKnownUntil: snapshot.LastKnownUntil}
	for _, id := range apikey.NormalizeProjectIDs(projects) {
		project, ok := projectScopedSnapshot(snapshot, id)
		if !ok {
			// Some old snapshots contain issues without project metadata.
			project = projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: id})
		}
		out.BoardIssues = append(out.BoardIssues, project.BoardIssues...)
		out.Pipeline = append(out.Pipeline, project.Pipeline...)
		out.Running = append(out.Running, project.Running...)
		out.Queue = append(out.Queue, project.Queue...)
		out.Blocked = append(out.Blocked, project.Blocked...)
		out.Completed = append(out.Completed, project.Completed...)
		out.Shipped = append(out.Shipped, project.Shipped...)
		out.TrackerUnavailable = append(out.TrackerUnavailable, project.TrackerUnavailable...)
		out.ForgeUnavailable = append(out.ForgeUnavailable, project.ForgeUnavailable...)
		out.FailureBreakers = append(out.FailureBreakers, project.FailureBreakers...)
		out.DispatchRecoveries = append(out.DispatchRecoveries, project.DispatchRecoveries...)
		out.Projects = append(out.Projects, telemetry.ProjectSnapshot{Project: project.Project, Counts: project.Counts, Tokens: project.Tokens, Throughput: project.Throughput})
		out.Counts.Running += project.Counts.Running
		out.Counts.Queue += project.Counts.Queue
		out.Counts.Blocked += project.Counts.Blocked
		out.Counts.Completed += project.Counts.Completed
		out.Tokens.Input += project.Tokens.Input
		out.Tokens.CachedInput += project.Tokens.CachedInput
		out.Tokens.Output += project.Tokens.Output
		out.Tokens.ReasoningOutput += project.Tokens.ReasoningOutput
		out.Tokens.Total += project.Tokens.Total
	}
	// BlockedRef has no project ownership field. Only retain references whose
	// identity is present in this authorized projection, never infer a grant
	// from a title, repository URL or an unscoped dependency note.
	visible := map[string]bool{}
	add := func(issue telemetry.Issue) {
		if issue.ID != "" {
			visible[issue.ID] = true
		}
		if issue.Identifier != "" {
			visible[issue.Identifier] = true
		}
	}
	for _, issue := range out.BoardIssues {
		add(issue)
	}
	for _, issue := range out.Pipeline {
		add(issue)
	}
	for _, issue := range out.Running {
		add(issue.Issue)
	}
	for _, issue := range out.Queue {
		add(issue.Issue)
	}
	for _, issue := range out.Blocked {
		add(issue.Issue)
	}
	for _, issue := range out.Completed {
		add(issue.Issue)
	}
	projectIssue := func(issue telemetry.Issue) telemetry.Issue {
		refs := make([]telemetry.BlockedRef, 0, len(issue.BlockedBy))
		for _, ref := range issue.BlockedBy {
			if visible[ref.ID] || visible[ref.Identifier] {
				refs = append(refs, ref)
			}
		}
		issue.BlockedBy, issue.DependencyNotes = refs, nil
		return issue
	}
	for i := range out.BoardIssues {
		out.BoardIssues[i] = projectIssue(out.BoardIssues[i])
	}
	for i := range out.Pipeline {
		out.Pipeline[i] = projectIssue(out.Pipeline[i])
	}
	for i := range out.Running {
		out.Running[i].Issue = projectIssue(out.Running[i].Issue)
	}
	for i := range out.Queue {
		out.Queue[i].Issue = projectIssue(out.Queue[i].Issue)
	}
	for i := range out.Blocked {
		out.Blocked[i].Issue = projectIssue(out.Blocked[i].Issue)
	}
	for i := range out.Completed {
		out.Completed[i].Issue = projectIssue(out.Completed[i].Issue)
	}
	return out
}
