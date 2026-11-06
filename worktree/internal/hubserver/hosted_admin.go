package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedAdministrator(c echo.Context) (apiCredential, error) {
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
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
		if err == nil && credential.HostedRole == "reporter" && !hostedReadRequest(c) {
			return s.nativeAPIError(c, operatortool.ErrAccessDenied)
		}
		if err != nil || credential.Hosted == nil || c.Param("organization") != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, nativeNotFound())
		}
		switch {
		case c.Path() == "/api/v2/organizations/:organization/usage/monthly" && c.Request().Method == http.MethodGet:
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" || !s.hostedAllUsageGrants(c.Request().Context(), credential) {
				return s.nativeAPIError(c, nativeNotFound())
			}
		case c.Path() == "/api/v2/organizations/:organization/projects" && c.Request().Method == http.MethodPost:
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return s.nativeAPIError(c, nativeNotFound())
			}
		case c.Path() == "/api/v2/organizations/:organization/runners/:runner/routing":
			if c.Request().Method == http.MethodGet {
				c.Set("hub_api_credential", credential)
				if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
					return s.nativeAPIError(c, err)
				}
				return next(c)
			}
			runner, err := s.readRunnerRouting(c.Request().Context(), nativeScope{organization: tracker.OrganizationID(c.Param("organization")), credential: credential}, c.Param("runner"))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if err := s.requireHostedRunnerProjects(c.Request().Context(), nil, credential, runnerManagementProjects(runner)); err != nil {
				if c.Request().Method == http.MethodGet {
					return s.nativeAPIError(c, nativeNotFound())
				}
				return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "not_found", Message: err.Error()})
			}
			credential.ManageRunners = true
		case strings.HasPrefix(c.Path(), enrollmentBase) || strings.HasPrefix(c.Path(), runnerBase) || c.Path() == "/api/v2/organizations/:organization/machines/:machine/routing":
			if err := s.requireHostedRunnerAdministration(c.Request().Context(), nil, credential); err != nil {
				return c.JSON(http.StatusNotFound, apiErrorResponse{Code: "not_found", Message: err.Error()})
			}
			credential.ManageRunners = true
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

func (s *Service) hostedAllUsageGrants(ctx context.Context, credential apiCredential) bool {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if credential.Hosted == nil {
		return false
	}
	var missing bool
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		missing, err = r.AccessMissingUsageGrant(ctx, credential.Hosted.Subject)
		return err
	})
	return err == nil && !missing
}

func (s *Service) hostedAllRunnerGrants(ctx context.Context, credential apiCredential) bool {
	if s.config.Hosted == nil {
		return credential.Scope == apiScopeAdmin && !credential.NativeOnly
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	return s.requireHostedRunnerAdministration(ctx, nil, credential) == nil
}

func (s *Service) requireHostedRunnerAdministration(ctx context.Context, query nativeQueryer, credential apiCredential) error {
	return s.requireHostedRunnerProjects(ctx, query, credential, nil)
}

func runnerManagementProjects(runner runnerauth.Runner) []tracker.ProjectID {
	if runner.Scope == "organization" {
		return nil
	}
	projects := slices.Clone(runner.ProjectIDs)
	if projects == nil {
		projects = []tracker.ProjectID{}
	}
	for _, lease := range runner.Leases {
		if !slices.Contains(projects, lease.ProjectID) {
			projects = append(projects, lease.ProjectID)
		}
	}
	return projects
}

func (s *Service) requireHostedRunnerProjects(ctx context.Context, query nativeQueryer, credential apiCredential, projects []tracker.ProjectID) error {
	refusal := fmt.Errorf("%w: Runner changes need manage_runner on every allowed project and every project with running work; ask an owner or admin", operatortool.ErrAccessDenied)
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" && credential.HostedRole != "member" {
		return refusal
	}
	if credential.HostedRole == "owner" || credential.HostedRole == "admin" {
		return nil
	}
	if projects != nil && len(projects) == 0 {
		return fmt.Errorf("%w: An unassigned runner needs an owner or admin", operatortool.ErrAccessDenied)
	}
	if s.config.TenantRepository != nil {
		return s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Serving, func(reader tenantstore.Reader) error {
			total := 0
			check := func(project corestore.Project) error {
				if project.DeletedAt != nil {
					return corestore.ErrNotFound
				}
				grant, err := reader.AccessProjectGrant(ctx, credential.Hosted.Subject, project.ID)
				if err != nil {
					return err
				}
				if !grant.ManageRunner {
					return refusal
				}
				total++
				return nil
			}
			if projects != nil {
				if len(projects) > corestore.MaxPageSize {
					return refusal
				}
				seen := make(map[tracker.ProjectID]bool, len(projects))
				for _, id := range projects {
					if seen[id] {
						return refusal
					}
					seen[id] = true
					project, err := reader.Project(ctx, id)
					if err != nil {
						return err
					}
					if err := check(project); err != nil {
						return err
					}
				}
			} else {
				page, err := reader.Projects(ctx, "", corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(page) >= corestore.MaxPageSize {
					return refusal
				}
				for _, project := range page {
					if project.DeletedAt == nil {
						if err := check(project); err != nil {
							return err
						}
					}
				}
			}
			if total == 0 {
				return refusal
			}
			return nil
		})
	}
	raw, err := sqlList(projects)
	if err != nil {
		return err
	}
	var project, name string
	err = queryRowSQL(ctx, query, `SELECT p.id,p.name FROM projects p WHERE p.deleted_at IS NULL AND p.organization_id=? AND (? OR p.id IN (SELECT value FROM sql_rows(?))) AND NOT EXISTS
(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=p.organization_id AND g.project_id=p.id AND g.user_id=? AND g.manage_runner=1)
ORDER BY p.name,p.id LIMIT 1`, s.config.Hosted.OrganizationID, projects == nil, raw, credential.Hosted.Subject).Scan(&project, &name)
	if err == nil {
		return fmt.Errorf("%w: project %q (%s) lacks a manage_runner grant; ask an owner or admin", operatortool.ErrAccessDenied, name, project)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var count int
	if err := queryRowSQL(ctx, query, "SELECT count(*) FROM projects WHERE deleted_at IS NULL AND organization_id=? AND (? OR id IN (SELECT value FROM sql_rows(?)))", s.config.Hosted.OrganizationID, projects == nil, raw).Scan(&count); err != nil {
		return err
	}
	if count == 0 || projects != nil && count != len(projects) {
		return refusal
	}
	return nil
}

func (s *Service) inviteHostedMember(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	credential, err := s.hostedAdministrator(c)
	role := c.FormValue("role")
	if err != nil || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot invite a member with this role")
	}
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || s.config.Hosted.SharedEntry == nil && hostedEmailListed(s.config.Hosted.StaffEmails, email) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter the customer's email address")
	}
	if err := s.validateHostedInvitationGrants(readCtx, nil, credential, email, nil); err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot replace this member's project access")
	}
	_, err = s.reserveHostedInvitationSeatFor(ctx, credential, email)
	if err != nil {
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedError(c, http.StatusTooManyRequests, limit.Error())
		}
		return s.hostedError(c, http.StatusServiceUnavailable, "The invitation could not be reserved")
	}
	if _, err := s.sendReservedHostedInvitationFor(ctx, credential, email, role, nil); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "The invitation could not be sent")
	}

	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) hostedManagedMemberFor(ctx context.Context, credential apiCredential, id string, removingOwner bool) (auth.Membership, error) {
	members, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
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
		if member.ID == id {
			selected = member
		}
	}
	if selected.ID == "" || selected.Role.Slug == "owner" && (credential.HostedRole != "owner" || removingOwner && owners <= 1) {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	return selected, nil
}

func (s *Service) revokeHostedMember(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot remove organization members")
	}
	if err := s.removeHostedMemberFor(c.Request().Context(), credential, c.Param("member")); err != nil {
		return s.hostedError(c, http.StatusForbidden, "This member could not be removed; the organization must retain an owner")
	}

	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) revokeHostedMemberLocally(ctx context.Context, user string) (resultErr error) {

	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.RevokeAccessMember(ctx, user, s.config.now()) })
}

func (s *Service) changeHostedRole(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	role := c.FormValue("role")
	if err != nil || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot assign this organization role")
	}
	if _, err := s.changeHostedRoleFor(c.Request().Context(), credential, c.Param("member"), role); err != nil {
		return s.hostedError(c, http.StatusForbidden, "This role could not be changed; the organization must retain an owner")
	}

	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) changeHostedGrant(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot manage project grants")
	}
	user, project := c.FormValue("user"), c.FormValue("project")
	if !hostedSafeID(user) || !hostedSafeID(project) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Select a member and project")
	}
	err = s.hostedGrant(c.Request().Context(), credential, user, project, hostedFormTrue(c, "write"), hostedFormTrue(c, "runner"), hostedFormTrue(c, "revoke"))
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "The project grant could not be changed")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) hostedGrant(ctx context.Context, credential apiCredential, user, project string, write, runner, revoke bool) (resultErr error) {

	return s.changeAccessGrant(ctx, credential, user, project, write, runner, revoke)
}

func (s *Service) createHostedProject(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot create projects")
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" || len(name) > 120 || !hostedFormTrue(c, "grant_access") {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter a project name and explicitly grant yourself project access")
	}
	project, err := s.createHostedProjectRecord(c.Request().Context(), credential, name, nil)
	if err != nil {
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedError(c, http.StatusTooManyRequests, limit.Error())
		}
		return s.hostedError(c, http.StatusConflict, "The project could not be created; check that its name is unique")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/projects/"+project))
}

func (s *Service) createHostedProjectRecord(ctx context.Context, credential apiCredential, name string, states []tracker.NativeState) (project string, resultErr error) {

	return s.createHostedTenantProject(ctx, credential, name, states)
}
func (s *Service) createHostedProjectInTx(ctx context.Context, tx *sql.Tx, credential apiCredential, name string, states []tracker.NativeState) (project string, resultErr error) {
	if err := s.recheckHostedMutation(ctx, tx, &nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}); err != nil {
		return "", err
	}
	if states != nil {
		if err := validateNativeStates(states); err != nil {
			return "", err
		}
	}
	err := tx.QueryRowContext(ctx, `SELECT p.id FROM projects p JOIN hosted_project_grants g ON g.project_id=p.id WHERE p.organization_id=? AND p.name=? AND g.user_id=? AND g.can_write=1`, s.config.Hosted.OrganizationID, name, credential.Hosted.Subject).Scan(&project)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	stamp := payloadTimestamp(ctx, s.config.now())
	before, err := s.database.hostedConsumption(ctx, tx, stamp)
	if err != nil {
		return "", err
	}
	if err := s.database.requireHostedFeature(ctx, tx, "collaboration", stamp); err != nil {
		return "", err
	}
	project = payloadNativeID(ctx, "prj")
	if states == nil {
		states = HostedProjectStates()
	}
	states = nativeTriageStates(states)
	if err := validateNativeStates(states); err != nil {
		return "", err
	}
	now := formatHubTime(payloadTimestamp(ctx, s.config.now()))
	encoded, err := marshalNative(states)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled,scheduling_rank) VALUES (?,?,?,'native',?,?,0,(SELECT COALESCE(max(scheduling_rank),-1)+1 FROM projects WHERE deleted_at IS NULL AND organization_id=?))", project, s.config.Hosted.OrganizationID, name, encoded, now, s.config.Hosted.OrganizationID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE organizations SET scheduling_revision = scheduling_revision + 1 WHERE id = ?", s.config.Hosted.OrganizationID); err != nil {
		return "", err
	}
	for _, state := range states {
		if _, err := tx.ExecContext(ctx, "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES (?,?,?,?,?,?,?)", project, state.Name, state.Name, state.Terminal, state.Dispatchable, now, now); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?,?,?) ON CONFLICT DO NOTHING", credential.ID, s.config.Hosted.OrganizationID, project); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_project_grants(user_id,organization_id,project_id,can_write,manage_runner)
SELECT user_id,?,?,1,1 FROM hosted_members WHERE active=1 AND role IN ('owner','admin')
ON CONFLICT(user_id,project_id) DO UPDATE SET can_write=1,manage_runner=1`, s.config.Hosted.OrganizationID, project); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO token_grants(token_id,organization_id,project_id)
SELECT principal_id,?,? FROM hosted_members WHERE active=1 AND role IN ('owner','admin')
ON CONFLICT(token_id,organization_id,project_id) DO NOTHING`, s.config.Hosted.OrganizationID, project); err != nil {
		return "", err
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, stamp, false); err != nil {
		return "", err
	}
	if err := s.hostedAuditWith(ctx, tx, credential.Hosted, "project_created", "create_hosted_project", project, http.StatusCreated); err != nil {
		return "", err
	}
	return project, nil
}

// HostedProjectStates is the workflow a new hosted project starts with. A
// completed run whose version the project's review policy already accepts,
// the default unless review.human asks for a person, moves from In
// Progress straight to Merging, where the runner that holds the project lands
// it and the Hub finishes it in Done. Human Review is where a Change Request
// waits for a person when the project requires review. Blocked holds a
// refused landing when human review is disabled. Neither lane ends nor
// dispatches the work, and neither is operator-only, so the orchestrator
// can move a finished run there. A person
// approves the change, which moves it to Merging, or sends it back to In
// Progress. Human Review is the default auto_promote.source_state, which is
// how the orchestrator names its review lane. Migrations 36, 38 and 39 move
// projects created from the earlier templates onto this one.
func HostedProjectStates() []tracker.NativeState {
	return nativeTriageStates([]tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done", "Cancelled"}},
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done", "Cancelled"}},
		{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done", "Cancelled"}},
		{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging", "Cancelled"}},
		{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress", "Cancelled"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
		{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}},
		{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}},
	})
}
