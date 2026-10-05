package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The hosted organization API (decisions section 12): the JSON the client's
// organization, member and fleet screens read and write. requireAPIScope cannot carry these routes:
// in hosted mode it rejects session credentials outside the native project
// base, so each handler authenticates the hosted session itself. The shared
// hosted boundary still supplies the CSRF check, the write lock and the
// organization path check.

const hostedOrganizationBase = "/api/v2/organizations/:organization"

func (s *Service) registerHostedOrganizationRoutes(e *echo.Echo) {
	session := s.hostedSessionOnly
	e.GET(hostedOrganizationBase+"/model-selection", s.getOrganizationModelSelection, session)
	e.PUT(hostedOrganizationBase+"/model-selection", s.updateOrganizationModelSelection, session)
	e.GET(hostedOrganizationBase+"/members", s.listHostedMembers, session)
	e.POST(hostedOrganizationBase+"/members/invitations", s.inviteHostedMemberJSON, session)
	e.PUT(hostedOrganizationBase+"/members/invitations/:invitation", s.editHostedInvitationJSON, session)
	e.DELETE(hostedOrganizationBase+"/members/invitations/:invitation", s.revokeHostedInvitationJSON, session)
	e.POST(hostedOrganizationBase+"/members/invitations/:invitation/resend", s.resendHostedInvitationJSON, session)
	e.DELETE(hostedOrganizationBase+"/members/:member", s.revokeHostedMemberJSON, session)
	e.PUT(hostedOrganizationBase+"/members/:member/role", s.changeHostedRoleJSON, session)
	e.PUT(hostedOrganizationBase+"/members/:member/grants", s.changeHostedGrantJSON, session)
	e.GET(hostedOrganizationBase+"/projects", s.listHostedProjects, session)
	e.GET(hostedOrganizationBase+"/fleet", s.hostedFleet, session)
	e.GET(hostedOrganizationBase+"/plan", s.hostedPlanJSON, session)
	e.POST(hostedOrganizationBase+"/switch", s.switchHostedOrganizationJSON, session)
	e.POST(hostedOrganizationBase+"/invitations/accept", s.acceptHostedInvitationJSON, session)
	e.POST(hostedOrganizationBase+"/support/start", s.startHostedSupportJSON, session)
}

// hostedSessionOnly refuses a bearer credential on the session API. These
// routes authenticate the hosted cookie, and the hosted boundary skips its
// CSRF check for bearer callers, so a mixed credential is never accepted.
func (s *Service) hostedSessionOnly(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
			return s.hostedJSONError(c, http.StatusForbidden, "This endpoint authenticates a hosted session")
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

type hostedMemberGrant = operatoradmin.ProjectGrant

type hostedMemberView struct {
	ID            string              `json:"id"`
	UserID        string              `json:"user_id"`
	Email         string              `json:"email"`
	Name          string              `json:"name,omitempty"`
	NeverSignedIn bool                `json:"never_signed_in"`
	Role          string              `json:"role"`
	Status        string              `json:"status"`
	Grants        []hostedMemberGrant `json:"grants"`
}

func (s *Service) hostedMemberIdentity(ctx context.Context, member auth.Membership, emails map[string]string) (hostedMemberView, error) {
	email, signedIn := emails[member.UserID]
	view := hostedMemberView{ID: member.ID, UserID: member.UserID, Email: email, NeverSignedIn: !signedIn, Role: member.Role.Slug, Status: member.Status, Grants: []hostedMemberGrant{}}
	if strings.TrimSpace(email) == "" {
		user, err := auth.LookupHostedUser(ctx, s.config.Hosted.Provider, member.UserID)
		if err != nil {
			return view, fmt.Errorf("read hosted member identity: %w", err)
		}
		view.Email, view.Name = user.Email, user.Name
	}
	return view, nil
}

type hostedInvitationView struct {
	ID        string              `json:"id"`
	Email     string              `json:"email"`
	Role      string              `json:"role"`
	CreatedAt string              `json:"created_at"`
	ExpiresAt string              `json:"expires_at"`
	Grants    []hostedMemberGrant `json:"grants"`
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
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	response, err := s.hostedMembersFor(c.Request().Context(), credential)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "action", "GET "+c.Path(), "", http.StatusOK); err != nil {
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

func (s *Service) hostedPendingInvitations(ctx context.Context) ([]hostedInvitationView, error) {
	invitations := []hostedInvitationView{}
	rows, err := s.database.db.QueryContext(ctx, `SELECT id, email, role, created_at, expires_at, grants_json
FROM hosted_invitations
WHERE organization_id = ? AND accepted_user_id = '' ORDER BY rowid DESC`, s.config.Hosted.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("read hosted invitations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var view hostedInvitationView
		var grants string
		if err := rows.Scan(&view.ID, &view.Email, &view.Role, &view.CreatedAt, &view.ExpiresAt, &grants); err != nil {
			return nil, fmt.Errorf("scan hosted invitation: %w", err)
		}
		if err := json.Unmarshal([]byte(grants), &view.Grants); err != nil {
			return nil, err
		}
		if view.ExpiresAt == "" {
			invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, view.ID)
			if err != nil {
				return nil, fmt.Errorf("read hosted invitation expiry: %w", err)
			}
			view.ExpiresAt = formatHubTime(invitation.ExpiresAt)
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
			member = current
		}
	}
	emails, err := s.hostedMemberEmails(ctx)
	if err != nil {
		return view, err
	}
	view, err = s.hostedMemberIdentity(ctx, member, emails)
	if err != nil {
		return view, err
	}
	grants, err := s.hostedMemberGrants(ctx)
	if err != nil {
		return view, err
	}
	if list, ok := grants[member.UserID]; ok {
		view.Grants = list
	}
	return view, nil
}

func (s *Service) editHostedInvitationJSON(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Grants []hostedMemberGrant `json:"grants"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot edit invitations")
	}
	if err := s.editHostedInvitationFor(c.Request().Context(), credential, c.Param("invitation"), request.Grants); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// inviteHostedMemberJSON answers POST /members/invitations.
func (s *Service) inviteHostedMemberJSON(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Email  string              `json:"email"`
		Role   string              `json:"role"`
		Grants []hostedMemberGrant `json:"grants"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(true); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil || !auth.ValidOrganizationRole(request.Role) || request.Role == "owner" && credential.HostedRole != "owner" {
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot invite a member with this role")
	}
	view, err := s.inviteHostedMemberFor(c.Request().Context(), credential, request.Email, request.Role, request.IdempotencyKey, request.Grants)
	if err != nil {
		var application *nativeError
		var limit *hostedLimitError
		switch {
		case errors.As(err, &application):
			return s.nativeAPIError(c, err)
		case errors.Is(err, mutation.ErrConflict):
			return s.nativeAPIError(c, &nativeError{Code: "idempotency_conflict", Message: mutation.ErrConflict.Error(), status: http.StatusConflict})
		case errors.Is(err, mutation.ErrUncertain):
			return c.JSON(http.StatusConflict, apiErrorResponse{Code: "idempotency_in_progress", Message: mutation.ErrUncertain.Error()})
		case errors.Is(err, operatortool.ErrInvalidArguments):
			return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter the customer's email address")
		case errors.As(err, &limit):
			return s.hostedJSONError(c, http.StatusTooManyRequests, limit.Error())
		default:
			return s.hostedJSONError(c, http.StatusServiceUnavailable, "The invitation could not be sent")
		}
	}
	return c.JSON(http.StatusCreated, view)
}

// hostedCommand identifies one idempotent hosted mutation: the actor's
// principal, the route it called and the key it sent, with the request whose
// hash a replay must match. It is stored in native_commands, the table every
// native mutation replays from.
type hostedCommand struct {
	organization          string
	actor, operation, key string
	input                 any
}

func (command hostedCommand) hash() (string, error) {
	encoded, err := json.Marshal(command.input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// hostedPendingCommand is the response a claimed key holds until its
// mutation completes. No real response has this shape.
const hostedPendingCommand = `{"detent_pending_command":true}`

// claimHostedCommand takes the key before the mutation runs. The insert is the
// claim: the primary key on native_commands lets exactly one request own a
// key. It reports true when the caller owns the key and should perform the
// mutation. Otherwise it has already answered: the stored response for a
// completed key, a conflict for a key used with different content, and a
// safe conflict for a key whose outcome is still pending or uncertain.
// A pending invitation is never taken over; inspect provider/application
// records before choosing a new key.
// claimHostedOperation is the application receipt path shared by adapters.
// Authorization must precede each call, including a replay.
func (s *Service) claimHostedOperation(ctx context.Context, command hostedCommand) (claimed bool, response json.RawMessage, err error) {
	command = command.forContext(ctx)
	hash, err := command.hash()
	if err != nil {
		return false, nil, err
	}
	result, err := s.database.db.ExecContext(ctx, `INSERT INTO native_commands (organization_id,actor_id,operation,command_key,request_hash,response_json,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, s.commandOrganization(ctx, command), command.actor, command.operation, command.key, hash, hostedPendingCommand, formatHubTime(s.config.now()))
	if err != nil {
		return false, nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	if count == 1 {
		return true, nil, nil
	}
	var storedHash, raw string
	if err := s.database.db.QueryRowContext(ctx, `SELECT request_hash,response_json FROM native_commands WHERE organization_id=? AND actor_id=? AND operation=? AND command_key=?`, s.commandOrganization(ctx, command), command.actor, command.operation, command.key).Scan(&storedHash, &raw); err != nil {
		return false, nil, err
	}
	if storedHash != hash {
		return false, nil, mutation.ErrConflict
	}
	if raw == hostedPendingCommand {
		return false, nil, mutation.ErrUncertain
	}
	return false, json.RawMessage(raw), nil
}

func (s *Service) claimHostedCommand(c echo.Context, command hostedCommand, status int) (bool, error) {
	claimed, response, err := s.claimHostedOperation(c.Request().Context(), command)
	if errors.Is(err, mutation.ErrConflict) {
		return false, s.nativeAPIError(c, &nativeError{Code: "idempotency_conflict", Message: mutation.ErrConflict.Error(), status: http.StatusConflict})
	}
	if errors.Is(err, mutation.ErrUncertain) {
		return false, c.JSON(http.StatusConflict, apiErrorResponse{Code: "idempotency_in_progress", Message: mutation.ErrUncertain.Error()})
	}
	if err != nil {
		return false, s.nativeAPIError(c, err)
	}
	if !claimed {
		return false, c.JSONBlob(status, response)
	}
	return true, nil
}

func (s *Service) completeHostedOperation(ctx context.Context, command hostedCommand, value any) (json.RawMessage, error) {
	command = command.forContext(ctx)
	response, err := marshalNative(value)
	if err != nil {
		return nil, err
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	result, err := s.database.db.ExecContext(persistCtx, `UPDATE native_commands SET response_json=? WHERE organization_id=? AND actor_id=? AND operation=? AND command_key=? AND response_json=?`, response, s.commandOrganization(ctx, command), command.actor, command.operation, command.key, hostedPendingCommand)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, mutation.ErrUncertain
	}
	return json.RawMessage(response), nil
}

// abandonHostedOperation gives back a claim whose mutation did not happen, so
// the same key can be retried.
func (s *Service) abandonHostedOperation(parent context.Context, command hostedCommand) {
	command = command.forContext(parent)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	if _, err := s.database.db.ExecContext(ctx, `DELETE FROM native_commands WHERE organization_id=? AND actor_id=? AND operation=? AND command_key=? AND response_json=?`, s.commandOrganization(ctx, command), command.actor, command.operation, command.key, hostedPendingCommand); err != nil {
		s.config.Logger.Warn("hosted command claim could not be released")
	}
}

// revokeHostedMemberJSON answers DELETE /members/:member. The organization always
// keeps an owner.
func (s *Service) revokeHostedMemberJSON(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot remove organization members")
	}
	if err := s.removeHostedMemberFor(c.Request().Context(), credential, c.Param("member")); err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "This member could not be removed; the organization must retain an owner")
	}
	return c.NoContent(http.StatusNoContent)
}

// changeHostedRoleJSON answers PUT /members/:member/role.
func (s *Service) changeHostedRoleJSON(c echo.Context) error {
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
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot assign this organization role")
	}
	view, err := s.changeHostedRoleFor(c.Request().Context(), credential, c.Param("member"), request.Role)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "This role could not be changed")
	}
	return c.JSON(http.StatusOK, view)
}

// changeHostedGrantJSON answers PUT /members/:member/grants. Authorization is
// the administrator check the grant form carries; the owner protection in
// hostedManagedMember guards membership changes, not project access.
func (s *Service) changeHostedGrantJSON(c echo.Context) error {
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
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot manage project grants")
	}
	view, err := s.changeHostedGrantFor(c.Request().Context(), credential, c.Param("member"), request.ProjectID, request.Write, request.Runner, request.Revoke)
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
	credential, _, err := s.hostedCredential(c.Request().Context(), c)
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
// The caller must grant itself access explicitly, as the project form does.
func (s *Service) createHostedProjectJSON(c echo.Context) error {
	var request struct {
		hostedIdempotent
		Name        string                `json:"name"`
		GrantAccess bool                  `json:"grant_access"`
		States      []tracker.NativeState `json:"states,omitempty"`
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
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter a project name and explicitly grant yourself project access")
	}
	if request.States != nil {
		if err := validateNativeStates(request.States); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	project, err := s.createHostedProjectRecord(c.Request().Context(), credential, name, request.States)
	if err != nil {
		var limit *hostedLimitError
		if errors.As(err, &limit) {
			return s.hostedJSONError(c, http.StatusTooManyRequests, limit.Error())
		}
		return s.hostedJSONError(c, http.StatusConflict, "The project could not be created; check that its name is unique")
	}
	scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), project: tracker.ProjectID(project), credential: credential}
	record, err := readNativeProject(c.Request().Context(), s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, record)
}

// hostedJSONError answers a hosted API failure with the native error shape.
func (s *Service) hostedJSONError(c echo.Context, status int, message string) error {
	return c.JSON(status, apiErrorResponse{Code: hostedErrorCode(status), Message: message})
}

func (s *Service) revokeHostedInvitationJSON(c echo.Context) error {
	var request hostedIdempotent
	if c.Request().ContentLength != 0 {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, err := s.hostedAdministrator(c); err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot revoke invitations")
	}
	if err := s.revokeHostedInvitationFor(c.Request().Context(), c.Param("invitation")); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.hostedJSONError(c, http.StatusNotFound, "This invitation is no longer pending")
		}
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "The invitation could not be revoked. Try again.")
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) resendHostedInvitationJSON(c echo.Context) error {
	var request hostedIdempotent
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "You cannot resend invitations")
	}
	if err := s.resendHostedInvitationFor(c.Request().Context(), credential, c.Param("invitation")); err != nil {
		var targetError *nativeError
		if errors.As(err, &targetError) {
			return s.nativeAPIError(c, err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return s.hostedJSONError(c, http.StatusNotFound, "This invitation is no longer pending")
		}
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "The invitation could not be resent. Try again.")
	}
	return c.NoContent(http.StatusNoContent)
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

// The HTTP and MCP application adapters share records. Context metadata is
// created by trusted authority; it is never decoded from hosted request bodies.
func (command hostedCommand) forContext(ctx context.Context) hostedCommand {
	if m, ok := mutation.FromContext(ctx); ok && m.RetryIdentity != "" {
		command.key = m.RetryIdentity
	}
	return command
}

func (s *Service) commandOrganization(ctx context.Context, command hostedCommand) string {
	if m, ok := mutation.FromContext(ctx); ok && m.OrganizationID != "" {
		return m.OrganizationID
	}
	if s.config.Hosted != nil {
		return s.config.Hosted.OrganizationID
	}
	return command.organization
}

// readHostedOperation is the read-only application receipt lookup. It never
// claims a missing command; action_result must remain a read operation.
func (s *Service) readHostedOperation(ctx context.Context, command hostedCommand) (json.RawMessage, bool, error) {
	command = command.forContext(ctx)
	hash, err := command.hash()
	if err != nil {
		return nil, false, err
	}
	var storedHash, raw string
	err = s.database.db.QueryRowContext(ctx, `SELECT request_hash,response_json FROM native_commands WHERE organization_id=? AND actor_id=? AND operation=? AND command_key=?`, s.commandOrganization(ctx, command), command.actor, command.operation, command.key).Scan(&storedHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if storedHash != hash {
		return nil, true, mutation.ErrConflict
	}
	if raw == hostedPendingCommand {
		return nil, true, mutation.ErrUncertain
	}
	return json.RawMessage(raw), true, nil
}
