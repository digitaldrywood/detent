package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) hostedAdministrator(c echo.Context) (hubapi.APICredential, error) {
	credential, status, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		if status >= http.StatusInternalServerError {
			return hubapi.APICredential{}, errors.Join(tenantauth.ErrUnavailable, err)
		}
		return hubapi.APICredential{}, err
	}
	if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return hubapi.APICredential{}, auth.ErrHostedIdentity
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
		return hubapi.APICredential{}, err
	}
	return credential, nil
}

func (s *Service) requireHostedAdministration(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		credential, status, err := s.authenticateAPIRequest(c)
		if err != nil && status >= http.StatusInternalServerError {
			return s.nativeAPIError(c, errors.Join(tenantauth.ErrUnavailable, err))
		}
		if err == nil && credential.HostedRole == "reporter" && !hubscope.HostedReadRequest(c) {
			return s.nativeAPIError(c, operatortool.ErrAccessDenied)
		}
		if err != nil || credential.Hosted == nil || c.Param("organization") != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
		switch {
		case c.Path() == "/api/v2/organizations/:organization/usage/monthly" && c.Request().Method == http.MethodGet:
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return s.nativeAPIError(c, hubapi.NativeNotFound())
			}
			if err := s.requireHostedUsageGrants(c.Request().Context(), credential); err != nil {
				if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
					return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
				}
				return s.nativeAPIError(c, hubapi.NativeNotFound())
			}
		case c.Path() == "/api/v2/organizations/:organization/projects" && c.Request().Method == http.MethodPost:
			if credential.HostedRole != "owner" && credential.HostedRole != "admin" {
				return s.nativeAPIError(c, hubapi.NativeNotFound())
			}
		case c.Path() == "/api/v2/organizations/:organization/runners/:runner/routing":
			if c.Request().Method == http.MethodGet {
				c.Set("hub_api_credential", credential)
				if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
					return s.nativeAPIError(c, err)
				}
				return next(c)
			}
			runner, err := s.readRunnerRouting(c.Request().Context(), hubscope.NativeScope{Organization: tracker.OrganizationID(c.Param("organization")), Credential: credential}, c.Param("runner"))
			if err != nil {
				return s.nativeAPIError(c, err)
			}
			if err := s.requireHostedRunnerProjects(c.Request().Context(), credential, runnerManagementProjects(runner)); err != nil {
				if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
					return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
				}
				if c.Request().Method == http.MethodGet {
					return s.nativeAPIError(c, hubapi.NativeNotFound())
				}
				return c.JSON(http.StatusNotFound, hubapi.APIErrorResponse{Code: "not_found", Message: err.Error()})
			}
			credential.ManageRunners = true
		case strings.HasPrefix(c.Path(), enrollmentBase) || strings.HasPrefix(c.Path(), runnerBase) || c.Path() == "/api/v2/organizations/:organization/machines/:machine/routing":
			if err := s.requireHostedRunnerAdministration(c.Request().Context(), credential); err != nil {
				if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
					return s.nativeAPIError(c, hubscope.OperatorAuthorizationError(err))
				}
				return c.JSON(http.StatusNotFound, hubapi.APIErrorResponse{Code: "not_found", Message: err.Error()})
			}
			credential.ManageRunners = true
		default:
			return s.nativeAPIError(c, hubapi.NativeNotFound())
		}
		c.Set("hub_api_credential", credential)
		if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "administration", c.Request().Method+" "+c.Path(), "", 0); err != nil {
			return s.nativeAPIError(c, err)
		}
		return next(c)
	}
}

func (s *Service) requireHostedUsageGrants(ctx context.Context, credential hubapi.APICredential) error {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if credential.Hosted == nil {
		return operatortool.ErrAccessDenied
	}
	var missing bool
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		missing, err = r.AccessMissingUsageGrant(ctx, credential.Hosted.Subject)
		return err
	})
	if err != nil {
		return hubscope.OperatorAuthorizationError(err)
	}
	if missing {
		return operatortool.ErrAccessDenied
	}
	return nil
}

func (s *Service) hostedAllRunnerGrants(ctx context.Context, credential hubapi.APICredential) bool {
	if s.config.Hosted == nil {
		return credential.Scope == hubapi.APIScopeAdmin && !credential.NativeOnly
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	return s.requireHostedRunnerAdministration(ctx, credential) == nil
}

func (s *Service) requireHostedRunnerAdministration(ctx context.Context, credential hubapi.APICredential) error {
	if credential.EntryKey != nil && (credential.EntryKey.Access != "all" || len(credential.EntryKey.BlockedProjects) > 0) {
		return operatortool.ErrAccessDenied
	}
	return s.requireHostedRunnerProjects(ctx, credential, nil)
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

func (s *Service) requireHostedRunnerProjects(ctx context.Context, credential hubapi.APICredential, projects []tracker.ProjectID) error {
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
	if s.config.TenantRepository == nil {
		return tenantauth.ErrUnavailable
	}
	return s.config.TenantRepository.ReadTenant(ctx, s.accessOrganization(), corestore.Serving, func(reader tenantstore.Reader) error {
		total, absent := 0, false
		var missing *corestore.Project
		check := func(project corestore.Project) error {
			if project.DeletedAt != nil {
				absent = true
				return nil
			}
			grant, err := reader.AccessProjectGrant(ctx, credential.Hosted.Subject, project.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err != nil || !grant.ManageRunner {
				if missing == nil || project.Name < missing.Name || project.Name == missing.Name && project.ID < missing.ID {
					missing = &project
				}
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
				if errors.Is(err, corestore.ErrNotFound) {
					absent = true
					continue
				}
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
		if missing != nil {
			return fmt.Errorf("%w: project %q (%s) lacks a manage_runner grant; ask an owner or admin", operatortool.ErrAccessDenied, missing.Name, missing.ID)
		}
		if absent || total == 0 {
			return refusal
		}
		return nil
	})
}

func (s *Service) inviteHostedMember(c echo.Context) error {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	credential, err := s.hostedAdministrator(c)
	if err != nil && !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
	}
	role := c.FormValue("role")
	if err != nil || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot invite a member with this role")
	}
	email := strings.ToLower(strings.TrimSpace(c.FormValue("email")))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || s.config.Hosted.SharedEntry == nil && hostedEmailListed(s.config.Hosted.StaffEmails, email) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter the customer's email address")
	}
	if err := s.validateHostedInvitationGrants(readCtx, credential, email, nil); err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
		}
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

func (s *Service) hostedManagedMemberFor(ctx context.Context, credential hubapi.APICredential, id string, removingOwner bool) (auth.Membership, error) {
	members, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
	if err != nil {
		return auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
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
	if err != nil && !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
	}
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot remove organization members")
	}
	if err := s.removeHostedMemberFor(c.Request().Context(), credential, c.Param("member")); err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
		}
		return s.hostedError(c, http.StatusForbidden, "This member could not be removed; the organization must retain an owner")
	}

	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) revokeHostedMemberLocally(ctx context.Context, user string) (resultErr error) {

	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return w.RevokeAccessMember(ctx, user, s.config.now()) })
}

func (s *Service) changeHostedRole(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil && !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
	}
	role := c.FormValue("role")
	if err != nil || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot assign this organization role")
	}
	if _, err := s.changeHostedRoleFor(c.Request().Context(), credential, c.Param("member"), role); err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
		}
		return s.hostedError(c, http.StatusForbidden, "This role could not be changed; the organization must retain an owner")
	}

	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) changeHostedGrant(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil && !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
	}
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot manage project grants")
	}
	user, project := c.FormValue("user"), c.FormValue("project")
	if !hostedSafeID(user) || !hostedSafeID(project) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Select a member and project")
	}
	err = s.hostedGrant(c.Request().Context(), credential, user, project, hostedFormTrue(c, "write"), hostedFormTrue(c, "runner"), hostedFormTrue(c, "revoke"))
	if err != nil {
		if !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
		}
		return s.hostedError(c, http.StatusForbidden, "The project grant could not be changed")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/organization"))
}

func (s *Service) hostedGrant(ctx context.Context, credential hubapi.APICredential, user, project string, write, runner, revoke bool) (resultErr error) {

	return s.changeAccessGrant(ctx, credential, user, project, write, runner, revoke)
}

func (s *Service) createHostedProject(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil && !errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization access is temporarily unavailable")
	}
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot create projects")
	}
	name := strings.TrimSpace(c.FormValue("name"))
	if name == "" || len(name) > 120 || !hostedFormTrue(c, "grant_access") {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter a project name and explicitly grant yourself project access")
	}
	project, err := s.createHostedProjectRecord(c.Request().Context(), credential, name, nil)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) {
			return s.hostedError(c, http.StatusServiceUnavailable, "The project could not be created. Try again.")
		}
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedError(c, http.StatusTooManyRequests, limit.Error())
		}
		if errors.Is(err, mutation.ErrConflict) {
			return s.hostedError(c, http.StatusConflict, "The project could not be created; check that its name is unique")
		}
		if errors.Is(hubscope.OperatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return s.hostedError(c, http.StatusForbidden, "You cannot create projects")
		}
		return s.hostedError(c, http.StatusServiceUnavailable, "The project could not be created. Try again.")
	}
	return c.Redirect(http.StatusSeeOther, s.hostedPath("/projects/"+project))
}

func (s *Service) createHostedProjectRecord(ctx context.Context, credential hubapi.APICredential, name string, states []tracker.NativeState) (project string, resultErr error) {

	return s.createHostedTenantProject(ctx, credential, name, states)
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
