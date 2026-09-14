package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedAdministrator(c echo.Context) (apiCredential, error) {
	credential, _, err := s.hostedCredential(c)
	if err != nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return apiCredential{}, auth.ErrHostedIdentity
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
		return apiCredential{}, err
	}
	return credential, nil
}

func (s *Service) requireHostedAdministration(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, _, err := s.authenticateAPIRequest(c)
		if err != nil || credential.Hosted == nil || c.Param("organization") != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, nativeNotFound())
		}
		switch {
		case c.Path() == "/api/v2/organizations/:organization/projects" && c.Request().Method == http.MethodPost:
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return s.nativeAPIError(c, nativeNotFound())
			}
		case strings.HasPrefix(c.Path(), enrollmentBase) || strings.HasPrefix(c.Path(), runnerBase) || c.Path() == "/api/v2/organizations/:organization/machines/:machine/routing":
			// The runner grant stays a permission of its own, separate from
			// the role: every member, owner included, needs it somewhere in
			// the organization. What changed is "somewhere" — it used to mean
			// every project. createRunnerEnrollment then re-checks it against
			// the projects the enrollment actually names.
			if credential.HostedRole == "viewer" || !s.hostedRunnerGrants(c.Request().Context(), credential, nil) {
				return s.nativeAPIError(c, nativeNotFound())
			}
			credential.ManageRunners = true
		case c.Path() == nativeBase+"/policy" && (c.Request().Method == http.MethodGet || c.Request().Method == http.MethodPut):
			// A hosted owner or admin reads and approves the project policy
			// descriptor here. Before this the only approval path a hosted
			// session had was the Templ-era /onboarding/policy route, so a
			// hosted owner could not clear `policy_mismatch` from the client.
			if !hostedAdministrationRole(credential) {
				return s.nativeAPIError(c, nativeNotFound())
			}
		default:
			return s.nativeAPIError(c, nativeNotFound())
		}
		c.Set("hub_api_credential", credential)
		if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
			return s.nativeAPIError(c, err)
		}
		return next(c)
	}
}

// hostedAdministrationRole reports whether the member administers the whole
// organization. An owner or an admin enrolls a runner for any project.
func hostedAdministrationRole(credential apiCredential) bool {
	return credential.HostedRole == "owner" || credential.HostedRole == "admin"
}

// hostedRunnerGrants reports whether the member may administer runners for the
// projects named. Enrollment used to demand the runner grant on *every*
// project in the organization, so a member granted exactly the projects an
// enrollment names was refused; the grant is per project, so the requirement
// is per project too.
//
// Naming no project asks the weaker question every runner route asks at the
// boundary: does this member hold the grant anywhere. An enrollment must name
// at least one project, because a runner grant authorizes nothing on its own
// and an empty list must never read as "every project".
func (s *Service) hostedRunnerGrants(ctx context.Context, credential apiCredential, projects []tracker.ProjectID) bool {
	if credential.Hosted == nil {
		return false
	}
	return hostedRunnerGrants(ctx, s.database.db, s.config.Hosted.OrganizationID, credential.Hosted.Subject, projects)
}

func hostedRunnerGrants(ctx context.Context, query nativeQueryer, organization, user string, projects []tracker.ProjectID) bool {
	const anyProject = `SELECT count(*) FROM hosted_project_grants g JOIN projects p ON p.id = g.project_id
WHERE g.user_id = ? AND g.manage_runner = 1 AND p.organization_id = ?`
	if len(projects) == 0 {
		var granted int
		if err := query.QueryRowContext(ctx, anyProject, user, organization).Scan(&granted); err != nil {
			return false
		}
		return granted > 0
	}
	for _, project := range projects {
		var granted int
		if err := query.QueryRowContext(ctx, anyProject+" AND p.id = ?", user, organization, project).Scan(&granted); err != nil || granted == 0 {
			return false
		}
	}
	return true
}

func (s *Service) hostedManagedMember(c echo.Context, credential apiCredential, removingOwner bool) (auth.Membership, error) {
	members, err := s.config.Hosted.Provider.Memberships(c.Request().Context(), "", credential.Hosted.OrganizationID)
	if err != nil {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	var selected auth.Membership
	owners := 0
	for _, member := range members {
		if member.OrganizationID != credential.Hosted.OrganizationID || member.Status != "active" {
			continue
		}
		if member.Role.Slug == "owner" {
			owners++
		}
		if member.ID == c.Param("member") {
			selected = member
		}
	}
	if selected.ID == "" || selected.Role.Slug == "owner" && (credential.HostedRole != "owner" || removingOwner && owners <= 1) {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	return selected, nil
}

func (s *Service) revokeHostedMemberLocally(ctx context.Context, user string) (resultErr error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	statements := []struct {
		query string
		args  []any
	}{
		{"UPDATE hosted_members SET active = 0,updated_at = ? WHERE user_id = ?", []any{formatHubTime(s.config.now()), user}},
		{"DELETE FROM token_grants WHERE token_id = (SELECT principal_id FROM hosted_members WHERE user_id = ?)", []any{user}},
		{"DELETE FROM hosted_project_grants WHERE user_id = ?", []any{user}},
		{"UPDATE hosted_sessions SET revoked_at = ? WHERE json_extract(identity_json,'$.subject') = ? AND revoked_at IS NULL", []any{formatHubTime(s.config.now()), user}},
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) hostedGrant(ctx context.Context, credential apiCredential, user, project string, write, runner, revoke bool) (resultErr error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential}); err != nil {
		return err
	}
	var principal string
	err = tx.QueryRowContext(ctx, "SELECT principal_id FROM hosted_members WHERE user_id = ? AND active = 1", user).Scan(&principal)
	if err != nil {
		return err
	}
	if revoke {
		if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_project_grants WHERE user_id = ? AND project_id = ?", user, project); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM token_grants WHERE token_id = ? AND project_id = ?", principal, project); err != nil {
			return err
		}
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner) VALUES (?,?,?,?,?) ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=excluded.can_write,manage_runner=excluded.manage_runner`, user, s.config.Hosted.OrganizationID, project, write, runner)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,?,?) ON CONFLICT DO NOTHING", principal, s.config.Hosted.OrganizationID, project); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) createHostedProjectRecord(ctx context.Context, credential apiCredential, name string) (project string, resultErr error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential}); err != nil {
		return "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN hosted_project_grants g ON g.project_id=p.id WHERE p.organization_id=? AND p.name=? AND g.user_id=? AND g.can_write=1`, s.config.Hosted.OrganizationID, name, credential.Hosted.Subject).Scan(&project)
	if err == nil {
		return project, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	stamp := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, stamp)
	if err != nil {
		return "", err
	}
	if err := s.database.requireHostedFeature(ctx, tx, "collaboration", stamp); err != nil {
		return "", err
	}
	project = newNativeID("prj")
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	now := formatHubTime(s.config.now())
	encoded, err := marshalNative(states)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) VALUES (?,?,?,'native',?,?,0)", project, s.config.Hosted.OrganizationID, name, encoded, now); err != nil {
		return "", err
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES (?,?,?,?,?,?,?)", project, state.Name, state.Name, state.Terminal, state.Dispatchable, now, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write) VALUES (?,?,?,1)", credential.Hosted.Subject, s.config.Hosted.OrganizationID, project); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,?,?)", credential.ID, s.config.Hosted.OrganizationID, project); err != nil {
		return "", err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, stamp, false); err != nil {
		return "", err
	}
	return project, tx.Commit()
}
