package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The hosted organization API (decisions section 12). Everything the removed
// Templ forms did is JSON here. requireAPIScope cannot carry these routes:
// in hosted mode it rejects session credentials outside the native project
// base, so each handler authenticates the hosted session itself. The shared
// hosted boundary still supplies the CSRF check, the write lock and the
// organization path check.

const hostedOrganizationBase = "/api/v2/organizations/:organization"

func (s *Service) registerHostedOrganizationRoutes(e *echo.Echo) {
	session := s.hostedSessionOnly
	e.GET(hostedOrganizationBase+"/members", s.listHostedMembers, session)
	e.POST(hostedOrganizationBase+"/members/invitations", s.inviteHostedMember, session)
	e.DELETE(hostedOrganizationBase+"/members/:member", s.revokeHostedMember, session)
	e.PUT(hostedOrganizationBase+"/members/:member/role", s.changeHostedRole, session)
	e.PUT(hostedOrganizationBase+"/members/:member/grants", s.changeHostedGrant, session)
	e.GET(hostedOrganizationBase+"/projects", s.listHostedProjects, session)
	e.GET(hostedOrganizationBase+"/fleet", s.hostedFleet, session)
}

// hostedSessionOnly refuses a bearer credential on the session API. These
// routes authenticate the hosted cookie, and the hosted boundary skips its
// CSRF check for bearer callers, so a mixed credential is never accepted.
func (s *Service) hostedSessionOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return s.hostedError(c, http.StatusForbidden, "This endpoint authenticates a hosted session")
		}
		return next(c)
	}
}

// hostedIdempotent is the body every hosted mutation accepts. The key makes a
// retry safe for the caller; handlers that create a record require it.
type hostedIdempotent struct {
	IdempotencyKey string `json:"idempotency_key"`
}

func (r hostedIdempotent) validate(required bool) error {
	key := strings.TrimSpace(r.IdempotencyKey)
	if key == "" {
		if required {
			return nativeInvalid("An idempotency key of at most 128 bytes is required")
		}
		return nil
	}
	if len(r.IdempotencyKey) > 128 {
		return nativeInvalid("An idempotency key of at most 128 bytes is required")
	}
	return nil
}

type hostedMemberGrant struct {
	ProjectID string `json:"project_id"`
	Write     bool   `json:"write"`
	Runner    bool   `json:"runner"`
}

type hostedMemberView struct {
	ID     string              `json:"id"`
	UserID string              `json:"user_id"`
	Email  string              `json:"email"`
	Role   string              `json:"role"`
	Status string              `json:"status"`
	Grants []hostedMemberGrant `json:"grants"`
}

type hostedInvitationView struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
}

type hostedMembersResponse struct {
	Members     []hostedMemberView     `json:"members"`
	Invitations []hostedInvitationView `json:"invitations"`
}

// hostedMemberRank orders the roles the members list groups by: the people who
// can change the organization come first, then everybody else.
func hostedMemberRank(role string) int {
	switch role {
	case "owner":
		return 0
	case "admin":
		return 1
	default:
		return 2
	}
}

// sortHostedMembers puts the members list in one order and keeps it there.
//
// The provider answers Memberships from a map, so its order is Go's randomized
// map order: the same organization came back owner-first on one read and
// viewer-first on the next, and the client renders the rows in response order,
// so the table reshuffled under the reader between refreshes. Owners and admins
// first, then by email, with the membership id breaking a tie so two members
// who share an address still have a fixed order.
func sortHostedMembers(members []hostedMemberView) {
	sort.Slice(members, func(i, j int) bool {
		left, right := members[i], members[j]
		if rank, other := hostedMemberRank(left.Role), hostedMemberRank(right.Role); rank != other {
			return rank < other
		}
		if left.Email != right.Email {
			return left.Email < right.Email
		}
		return left.ID < right.ID
	})
}

// listHostedMembers answers GET /members. Owners and admins see the whole
// organization with its pending invitations; anybody else sees only their own
// membership.
func (s *Service) listHostedMembers(c echo.Context) error {
	credential, _, err := s.hostedCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx := c.Request().Context()
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Organization membership is temporarily unavailable")
	}
	emails, err := s.hostedMemberEmails(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	grants, err := s.hostedMemberGrants(ctx)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	response := hostedMembersResponse{Members: []hostedMemberView{}, Invitations: []hostedInvitationView{}}
	for _, member := range memberships {
		if member.OrganizationID != credential.Hosted.OrganizationID || member.Status != "active" {
			continue
		}
		if !manage && member.UserID != credential.Hosted.Subject {
			continue
		}
		view := hostedMemberView{ID: member.ID, UserID: member.UserID, Email: emails[member.UserID], Role: member.Role.Slug, Status: member.Status, Grants: []hostedMemberGrant{}}
		if list, ok := grants[member.UserID]; ok {
			view.Grants = list
		}
		response.Members = append(response.Members, view)
	}
	sortHostedMembers(response.Members)
	if manage {
		response.Invitations, err = s.hostedPendingInvitations(ctx)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, response)
}

func (s *Service) hostedMemberEmails(ctx context.Context) (map[string]string, error) {
	emails := map[string]string{}
	rows, err := s.database.db.QueryContext(ctx, "SELECT user_id, email FROM hosted_members WHERE active = 1")
	if err != nil {
		return nil, fmt.Errorf("read hosted members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var user, email string
		if err := rows.Scan(&user, &email); err != nil {
			return nil, fmt.Errorf("scan hosted member: %w", err)
		}
		emails[user] = email
	}
	return emails, errors.Join(rows.Err(), rows.Close())
}

func (s *Service) hostedMemberGrants(ctx context.Context) (map[string][]hostedMemberGrant, error) {
	grants := map[string][]hostedMemberGrant{}
	rows, err := s.database.db.QueryContext(ctx, "SELECT user_id, project_id, can_write, manage_runner FROM hosted_project_grants WHERE organization_id = ? ORDER BY user_id, project_id", s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("read hosted project grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var user string
		var grant hostedMemberGrant
		if err := rows.Scan(&user, &grant.ProjectID, &grant.Write, &grant.Runner); err != nil {
			return nil, fmt.Errorf("scan hosted project grant: %w", err)
		}
		grants[user] = append(grants[user], grant)
	}
	return grants, errors.Join(rows.Err(), rows.Close())
}

// hostedPendingInvitations lists invitations that nobody has accepted. The
// expiry comes from the reserved member seat, which is what the allowance
// check holds open for the invited address.
func (s *Service) hostedPendingInvitations(ctx context.Context) ([]hostedInvitationView, error) {
	invitations := []hostedInvitationView{}
	rows, err := s.database.db.QueryContext(ctx, `SELECT i.id, i.email, i.role, i.created_at, COALESCE(r.expires_at, 0)
FROM hosted_invitations i LEFT JOIN hosted_member_reservations r ON r.email = i.email
WHERE i.organization_id = ? AND i.accepted_user_id = '' ORDER BY i.rowid`, s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("read hosted invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var view hostedInvitationView
		var expires int64
		if err := rows.Scan(&view.ID, &view.Email, &view.Role, &view.CreatedAt, &expires); err != nil {
			return nil, fmt.Errorf("scan hosted invitation: %w", err)
		}
		if expires > 0 {
			view.ExpiresAt = time.Unix(expires, 0).UTC().Format(time.RFC3339)
		}
		invitations = append(invitations, view)
	}
	return invitations, errors.Join(rows.Err(), rows.Close())
}

// hostedMemberResponse reads one membership back so a role or grant change
// answers with the record the client should render.
func (s *Service) hostedMemberResponse(ctx context.Context, member auth.Membership) (hostedMemberView, error) {
	view := hostedMemberView{ID: member.ID, UserID: member.UserID, Role: member.Role.Slug, Status: member.Status, Grants: []hostedMemberGrant{}}
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, member.UserID, member.OrganizationID)
	if err != nil {
		return view, fmt.Errorf("read hosted membership: %w", err)
	}
	for _, current := range memberships {
		if current.ID == member.ID {
			view.Role, view.Status = current.Role.Slug, current.Status
		}
	}
	emails, err := s.hostedMemberEmails(ctx)
	if err != nil {
		return view, err
	}
	view.Email = emails[member.UserID]
	grants, err := s.hostedMemberGrants(ctx)
	if err != nil {
		return view, err
	}
	if list, ok := grants[member.UserID]; ok {
		view.Grants = list
	}
	return view, nil
}

// inviteHostedMember answers POST /members/invitations.
func (s *Service) inviteHostedMember(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(true); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil || !auth.ValidOrganizationRole(request.Role) || request.Role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot invite a member with this role")
	}
	email := strings.ToLower(strings.TrimSpace(request.Email))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || hostedEmailListed(s.config.Hosted.StaffEmails, email) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter the customer's email address")
	}
	if err := s.reserveHostedInvitation(c.Request().Context(), email); err != nil {
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedError(c, http.StatusTooManyRequests, limit.Error())
		}
		return s.hostedError(c, http.StatusServiceUnavailable, "The invitation could not be reserved")
	}
	invitation, err := s.config.Hosted.Provider.Invite(c.Request().Context(), credential.Hosted.OrganizationID, email, request.Role, credential.Hosted.Subject)
	if err != nil || invitation.OrganizationID != credential.Hosted.OrganizationID || !strings.EqualFold(invitation.Email, email) || invitation.State != "pending" {
		return s.hostedInvitationFailure(c, email, err)
	}
	created := formatHubTime(s.config.now())
	_, err = s.database.db.ExecContext(c.Request().Context(), `INSERT INTO hosted_invitations(id,email,organization_id,role,created_at) VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, invitation.ID, email, s.config.Hosted.OrganizationID, request.Role, created)
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "The invitation could not be recorded")
	}
	view := hostedInvitationView{ID: invitation.ID, Email: email, Role: request.Role, CreatedAt: created}
	if !invitation.ExpiresAt.IsZero() {
		view.ExpiresAt = invitation.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return c.JSON(http.StatusCreated, view)
}

// revokeHostedMember answers DELETE /members/:member. The organization always
// keeps an owner.
func (s *Service) revokeHostedMember(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot remove organization members")
	}
	member, err := s.hostedManagedMember(c, credential, true)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "This member cannot be removed; the organization must retain an owner")
	}
	if err := s.revokeHostedMemberLocally(c.Request().Context(), member.UserID); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Membership removal is temporarily unavailable")
	}
	// The member's local access is gone, so every relay connection they hold
	// goes with it (decisions section 18.2): a socket that outlived the
	// membership would be exactly the case the re-check exists to prevent.
	s.authorityChanged(c.Request().Context(), member.UserID)
	if err := s.config.Hosted.Provider.RevokeMembership(c.Request().Context(), member.ID); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Local access is revoked. Provider revocation could not be confirmed; retry removal.")
	}
	return c.NoContent(http.StatusNoContent)
}

// changeHostedRole answers PUT /members/:member/role.
func (s *Service) changeHostedRole(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Role string `json:"role"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil || !auth.ValidOrganizationRole(request.Role) || request.Role == "owner" && credential.HostedRole != "owner" {
		return s.hostedError(c, http.StatusForbidden, "You cannot assign this organization role")
	}
	member, err := s.hostedManagedMember(c, credential, request.Role != "owner")
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "This role cannot be changed; the organization must retain an owner")
	}
	if err := s.config.Hosted.Provider.SetMembershipRole(c.Request().Context(), member.ID, request.Role); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "The role could not be changed")
	}
	if _, err := s.database.db.ExecContext(c.Request().Context(), "UPDATE hosted_members SET role = ?,updated_at = ? WHERE user_id = ?", request.Role, formatHubTime(s.config.now()), member.UserID); err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "The role could not be recorded")
	}
	s.authorityChanged(c.Request().Context(), member.UserID)
	view, err := s.hostedMemberResponse(c.Request().Context(), member)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

// changeHostedGrant answers PUT /members/:member/grants. Authorization is the
// administrator check the removed form carried; the owner protection in
// hostedManagedMember guards membership changes, not project access.
func (s *Service) changeHostedGrant(c echo.Context) error {
	var request struct {
		hostedIdempotent
		ProjectID string `json:"project_id"`
		Write     bool   `json:"write"`
		Runner    bool   `json:"runner"`
		Revoke    bool   `json:"revoke"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "You cannot manage project grants")
	}
	member, err := s.hostedMemberByID(c.Request().Context(), credential, c.Param("member"))
	if err != nil {
		return s.hostedError(c, http.StatusNotFound, "This member is not part of the organization")
	}
	if !hostedSafeID(member.UserID) || !hostedSafeID(request.ProjectID) {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Select a member and project")
	}
	if err := s.hostedGrant(c.Request().Context(), credential, member.UserID, request.ProjectID, request.Write, request.Runner, request.Revoke); err != nil {
		return s.hostedError(c, http.StatusForbidden, "The project grant could not be changed")
	}
	s.authorityChanged(c.Request().Context(), member.UserID)
	view, err := s.hostedMemberResponse(c.Request().Context(), member)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, view)
}

// hostedMemberByID resolves one active membership of this organization.
func (s *Service) hostedMemberByID(ctx context.Context, credential apiCredential, id string) (auth.Membership, error) {
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
	if err != nil {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	for _, member := range memberships {
		if member.ID == id && member.OrganizationID == credential.Hosted.OrganizationID && member.Status == "active" {
			return member, nil
		}
	}
	return auth.Membership{}, auth.ErrHostedIdentity
}

// listHostedProjects answers GET /projects with the readable projects and the
// onboarding readiness each project screen opens with.
func (s *Service) listHostedProjects(c echo.Context) error {
	credential, _, err := s.hostedCredential(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx := c.Request().Context()
	readable, err := s.hostedReadableProjects(ctx, credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	projects := make([]hostedProjectView, 0, len(readable))
	for _, project := range readable {
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project.ID), credential: credential}
		setup, err := s.projectOnboarding(ctx, scope)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		view := hostedProjectView{appBootstrapProject: project}
		view.Onboarding.Ready, view.Onboarding.Steps = setup.Ready, setup.Steps
		if view.Onboarding.Steps == nil {
			view.Onboarding.Steps = []onboarding.Step{}
		}
		projects = append(projects, view)
	}
	if err := s.hostedAudit(ctx, credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, projects)
}

// hostedProjectView is one readable project with the readiness the project
// screen opens with. The steps keep the shape the onboarding endpoint already
// returns so the client has one representation of readiness.
type hostedProjectView struct {
	appBootstrapProject
	Onboarding struct {
		Ready bool              `json:"ready"`
		Steps []onboarding.Step `json:"steps"`
	} `json:"onboarding"`
}

// createHostedProjectJSON answers POST /projects for a hosted owner or admin.
// The caller must grant itself access explicitly, as the removed form did.
func (s *Service) createHostedProjectJSON(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Name        string `json:"name"`
		GrantAccess bool   `json:"grant_access"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(true); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, ok := c.Get("hub_api_credential").(apiCredential)
	if !ok || credential.Hosted == nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	name := strings.TrimSpace(request.Name)
	if name == "" || len(name) > 120 || !request.GrantAccess {
		return s.hostedError(c, http.StatusUnprocessableEntity, "Enter a project name and explicitly grant yourself project access")
	}
	project, err := s.createHostedProjectRecord(c.Request().Context(), credential, name)
	if err != nil {
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedError(c, http.StatusTooManyRequests, limit.Error())
		}
		return s.hostedError(c, http.StatusConflict, "The project could not be created; check that its name is unique")
	}
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
	record, err := readNativeProject(c.Request().Context(), s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, record)
}

// hostedAPIError maps a credential failure onto the section 12 error shape.
func (s *Service) hostedAPIError(c echo.Context, err error) error {
	if errors.Is(err, auth.ErrInvalidSession) {
		return c.JSON(http.StatusUnauthorized, apiErrorResponse{Code: "unauthorized", Message: "A hosted session is required"})
	}
	if errors.Is(err, auth.ErrHostedIdentity) {
		return c.JSON(http.StatusForbidden, apiErrorResponse{Code: "forbidden", Message: "This account has no access to this organization"})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	return s.nativeAPIError(c, err)
}

func hostedErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request"
	case http.StatusTooManyRequests:
		return "allowance_exhausted"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "request_failed"
	}
}
