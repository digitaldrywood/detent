package hubserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const hostedCookie = "detent_hosted_session"

func (s *Service) CreateWebSession(ctx context.Context, record auth.SessionRecord) error {
	return s.createAccessWebSession(ctx, record)
}

func (s *Service) storedWebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	return s.accessWebSession(ctx, hash, now)
}

func (s *Service) WebSession(ctx context.Context, hash string, now time.Time) (auth.Session, error) {
	session, err := s.storedWebSession(ctx, hash, now)
	if err != nil {
		return auth.Session{}, err
	}
	current, err := s.config.Hosted.Provider.CurrentSession(ctx, *session.Identity)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return auth.Session{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !current.ExpiresAt.After(now) || current.Subject != session.Identity.Subject || current.SessionID != session.Identity.SessionID || current.OrganizationID != session.Identity.OrganizationID || current.SupportActor != session.Identity.SupportActor || current.SupportReason != session.Identity.SupportReason {
		return auth.Session{}, auth.ErrInvalidSession
	}
	if current.ExpiresAt.Before(session.ExpiresAt) {
		session.ExpiresAt = current.ExpiresAt
	}
	session.Identity.ExpiresAt = session.ExpiresAt
	if current.SupportActor != "" && (!hostedEmailListed(s.config.Hosted.SupportActors, current.SupportActor) || !auth.ValidSupportReason(current.SupportReason)) {
		return auth.Session{}, auth.ErrInvalidSession
	}
	return session, nil
}

func (s *Service) hostedSession(ctx context.Context, c echo.Context) (auth.Session, string, error) {
	if s.hostedShared() {
		return s.sharedHostedSession(ctx, c)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil || s.hostedSessions == nil {
		return auth.Session{}, "", auth.ErrInvalidSession
	}
	session, err := s.hostedSessions.Authenticate(ctx, cookie.Value)
	if err == nil {
		c.Set("hosted_session", session)
	}
	return session, apikey.HashToken(cookie.Value), err
}

func (s *Service) hostedMembership(ctx context.Context, identity *auth.HostedIdentity) (auth.Membership, error) {
	if identity == nil || identity.OrganizationID == "" {
		return auth.Membership{}, auth.ErrHostedIdentity
	}
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, identity.Subject, identity.OrganizationID)
	if err != nil {
		return auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	for _, membership := range memberships {
		if membership.UserID == identity.Subject && membership.OrganizationID == identity.OrganizationID && membership.Status == "active" && auth.ValidOrganizationRole(membership.Role.Slug) {
			return membership, nil
		}
	}
	return auth.Membership{}, auth.ErrHostedIdentity
}

func (s *Service) hostedCredential(ctx context.Context, c echo.Context) (apiCredential, int, error) {
	session, hash, err := s.hostedSession(ctx, c)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable || auth.HostedIdentityReason(err) == auth.HostedReasonProviderInvalid {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		if errors.Is(err, auth.ErrInvalidSession) {
			return apiCredential{}, http.StatusUnauthorized, auth.ErrInvalidSession
		}
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	var credential apiCredential
	var status int
	read := func() (apiCredential, int, error) {
		if claims, ok := hostedSharedClaims(c); ok && claims.Kind == cloudassert.KindBrowser {
			return s.hostedSharedCredential(ctx, session, hash, claims.Role)
		}
		return s.hostedSessionCredential(ctx, session, hash)
	}
	credential, status, err = read()
	if status == http.StatusForbidden {
		completed, completionErr := s.completePendingHostedInvitation(ctx, session)
		if completionErr != nil {
			return apiCredential{}, http.StatusServiceUnavailable, completionErr
		}
		if completed {
			credential, status, err = read()
		}
	}
	if err != nil && status >= http.StatusInternalServerError {
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err == nil {
		c.Set("hub_api_credential", credential)
	}
	return credential, status, err
}

// hostedSharedCredential builds a browser credential from the shared entry's
// assertion. The entry verified the WorkOS access token and carries its role,
// so the tenant does not call the provider on the request path; membership
// removal reaches the tenant through the entry's revocation of the binding.
func (s *Service) hostedSharedCredential(ctx context.Context, session auth.Session, hash, role string) (apiCredential, int, error) {
	var member accessstore.Member
	err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(ctx, session.Identity.Subject)
		return err
	})
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return apiCredential{}, http.StatusServiceUnavailable, err
	}
	if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, accessstore.ErrDenied) || errors.Is(err, auth.ErrHostedIdentity) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	if err != nil {
		return apiCredential{}, http.StatusServiceUnavailable, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if !member.Active || !auth.ValidOrganizationRole(role) {
		return apiCredential{}, http.StatusForbidden, auth.ErrHostedIdentity
	}
	membership := auth.Membership{ID: member.MembershipID}
	membership.Role.Slug = lesserHostedRole(role, member.Role)
	return s.accessMemberCredential(ctx, session, hash, membership)
}

// completePendingHostedInvitation finishes an invitation the provider already
// accepted for this subject but whose tenant membership was never written, such
// as a join interrupted between the provider acceptance and the tenant
// transaction. A pending invitation still needs the explicit join. The
// acceptance path re-verifies the provider invitation, subject, membership and
// role, and its conditional update admits one acceptance, so no shared lock is
// held across the provider calls. It reports true whenever completion was
// attempted, since a concurrent request may have completed it first.
func (s *Service) completePendingHostedInvitation(ctx context.Context, session auth.Session) (bool, error) {
	if session.Identity == nil || session.Identity.SupportActor != "" || session.Email == "" || hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
		return false, nil
	}
	readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	var id string
	var err error
	var latest time.Time
	after := ""
	err = s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
		for {
			page, err := r.AccessInvitations(readCtx, after, 200)
			if err != nil {
				return err
			}
			for _, row := range page {
				if row.AcceptedUserID == "" && strings.EqualFold(row.Email, session.Email) && row.ExpiresAt.After(s.config.now()) && row.CreatedAt.After(latest) {
					id, latest = row.ID, row.CreatedAt
				}
			}
			if len(page) < 200 {
				return nil
			}
			after = page[len(page)-1].ID
		}
	})
	if err == nil && id == "" {
		err = sql.ErrNoRows
	}
	cancel()
	if err != nil {
		if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return false, operatorAuthorizationError(err)
		}
		return false, nil
	}
	invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, id)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return false, operatorAuthorizationError(err)
	}
	if err != nil || invitation.State != "accepted" || invitation.AcceptedUserID != session.Identity.Subject {
		return false, nil
	}
	if err := s.acceptHostedInvitationIDFor(ctx, auth.Identity{Subject: session.Identity.Subject, Email: session.Email, EmailVerified: true, Hosted: session.Identity}, id); err != nil {
		if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return false, operatorAuthorizationError(err)
		}
	}
	return true, nil
}

// lesserHostedRole returns the less privileged of two organization roles, so
// a downgrade recorded by the tenant applies before the entry's access token
// carrying the older role expires.
func lesserHostedRole(a, b string) string {
	rank := map[string]int{"viewer": 0, "reporter": 1, "member": 2, "admin": 3, "owner": 4}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}

func (s *Service) hostedSessionCredential(ctx context.Context, session auth.Session, hash string) (apiCredential, int, error) {
	membership, err := s.hostedMembership(ctx, session.Identity)
	if err != nil {
		if errors.Is(err, tenantauth.ErrUnavailable) || auth.HostedIdentityReason(err) == auth.HostedReasonProviderUnavailable {
			return apiCredential{}, http.StatusServiceUnavailable, err
		}
		return apiCredential{}, http.StatusForbidden, err
	}
	return s.hostedMemberCredential(ctx, session, hash, membership)
}

func (s *Service) hostedMemberCredential(ctx context.Context, session auth.Session, hash string, membership auth.Membership) (apiCredential, int, error) {
	return s.accessMemberCredential(ctx, session, hash, membership)
}

func hostedCSRF(token string) string {
	sum := sha256.Sum256([]byte("detent-hosted-csrf:" + token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) hostedCSRFValid(c echo.Context) bool {
	value := c.Request().Header.Get("X-CSRF-Token")
	if value == "" && strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationForm) {
		c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxAPIRequestBodyBytes)
		value = c.FormValue("csrf")
	}
	if s.hostedShared() {
		return s.hostedSharedCSRFValid(c, value)
	}
	cookie, err := c.Cookie(hostedCookie)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(hostedCSRF(cookie.Value))) == 1
}

func hostedReadRequest(c echo.Context) bool {
	return c.Request().Method == http.MethodGet || c.Request().Method == http.MethodHead
}

type hostedMutationContext struct{}

func (s *Service) hostedBoundary(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		defer s.recordHostedRequest(c, time.Now())
		c.Response().Header().Set("Cache-Control", "no-store")
		c.Response().Header().Set("Referrer-Policy", "no-referrer")
		c.Response().Header().Set("X-Content-Type-Options", "nosniff")
		if organization := c.Param("organization"); organization != "" && organization != s.config.Hosted.OrganizationID {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if strings.HasPrefix(c.Path(), "/api/v1/") {
			if s.database.postgres != nil {
				return s.postgresFeatureUnavailable(c)
			}
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/health" && s.database.postgres == nil {
			return s.nativeAPIError(c, nativeNotFound())
		}
		if c.Path() == "/webhooks/stripe" && c.Request().Method == http.MethodPost {
			return next(c)
		}
		bearerAPI := (strings.HasPrefix(c.Request().URL.Path, "/api/v2/") || c.Path() == "/mcp") && c.Request().Header.Get(echo.HeaderAuthorization) != ""
		if claims, ok := hostedSharedClaims(c); ok && (claims.Kind == cloudassert.KindService || claims.Kind == cloudassert.KindKey) {
			return next(c)
		}
		if !hostedReadRequest(c) && !bearerAPI && !strings.HasSuffix(c.Path(), "/redeem") && !s.hostedCSRFValid(c) {
			return s.refusedAPIError(c, http.StatusForbidden, "invalid_csrf", "Reload the form and try again", apiErrorResponse{Code: "invalid_csrf", Message: "Reload the form and try again"})
		}
		if !hostedReadRequest(c) && !bearerAPI {
			s.hostedMutationMu.Lock()
			defer s.hostedMutationMu.Unlock()
			c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), hostedMutationContext{}, true)))
		}
		return next(c)
	}
}

func (s *Service) requireHostedProject(ctx context.Context, query nativeQueryer, scope nativeScope, write bool) error {
	if s.config.AccessRepository != nil {
		return s.accessRead(ctx, scope.organization, func(r tenantstore.Reader) error { return s.authorizeAccessScope(ctx, r, scope, write) })
	}
	if s.config.Hosted == nil {
		return authorizeNativeProject(ctx, query, scope)
	}
	if scope.credential.EntryKey != nil {
		if !scope.credential.EntryKey.Allows(string(scope.project)) {
			return &apikey.Refusal{Code: "key_project_denied", Message: "The project is outside this key's access context"}
		}
		if scope.credential.EntryKey.Kind == "service" && string(scope.organization) == s.config.Hosted.OrganizationID {
			return nil
		}
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	if string(scope.organization) != s.config.Hosted.OrganizationID || scope.project == "" {
		return nativeNotFound()
	}
	var count int
	err := query.QueryRowContext(ctx, `SELECT count(*) FROM hosted_project_grants g JOIN hosted_members m ON m.user_id = g.user_id
WHERE m.user_id = ? AND m.active = 1 AND g.organization_id = ? AND g.project_id = ? AND (? = 0 OR g.can_write = 1)`, scope.credential.Hosted.Subject, scope.organization, scope.project, write).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 || write && scope.credential.HostedRole == "viewer" {
		return nativeNotFound()
	}
	return nil
}

// hostedMutationIdentity returns the identity and membership a mutation is
// rechecked against. Behind the shared entry the credential already comes
// from this request's entry assertion, whose access token the entry verified,
// so it is used as is; a dedicated tenant asks the provider.
func (s *Service) hostedMutationIdentity(ctx context.Context, credential apiCredential) (auth.HostedIdentity, auth.Membership, error) {
	if credential.EntryKey != nil {
		membership := auth.Membership{ID: credential.HostedMembership}
		membership.Role.Slug = credential.HostedRole
		if !credential.Hosted.ExpiresAt.After(s.config.now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if credential.HostedKeyScope != "" {
		membership, err := s.hostedMembership(ctx, credential.Hosted)
		if err != nil {
			return auth.HostedIdentity{}, auth.Membership{}, err
		}
		if membership.ID != credential.HostedMembership {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	if s.hostedShared() {
		var membership auth.Membership
		membership.ID, membership.Role.Slug = credential.HostedMembership, credential.HostedRole
		if membership.ID == "" || !auth.ValidOrganizationRole(membership.Role.Slug) || !credential.Hosted.ExpiresAt.After(s.config.now()) {
			return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
		}
		return *credential.Hosted, membership, nil
	}
	identity, err := s.config.Hosted.Provider.CurrentSession(ctx, *credential.Hosted)
	if err != nil && !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
		return auth.HostedIdentity{}, auth.Membership{}, errors.Join(tenantauth.ErrUnavailable, err)
	}
	if err != nil || !identity.ExpiresAt.After(s.config.now()) || identity.Subject != credential.Hosted.Subject || identity.SessionID != credential.Hosted.SessionID || identity.SupportActor != credential.Hosted.SupportActor || identity.SupportReason != credential.Hosted.SupportReason || identity.OrganizationID != credential.Hosted.OrganizationID {
		return auth.HostedIdentity{}, auth.Membership{}, auth.ErrHostedIdentity
	}
	membership, err := s.hostedMembership(ctx, credential.Hosted)
	if err != nil {
		return auth.HostedIdentity{}, auth.Membership{}, err
	}
	return identity, membership, nil
}

func (s *Service) recheckHostedMutation(ctx context.Context, tx *sql.Tx, scope *nativeScope) error {
	return s.recheckHostedAuthority(ctx, tx, scope, apikey.ScopeWrite)
}

func (s *Service) recheckHostedAuthority(ctx context.Context, tx *sql.Tx, scope *nativeScope, required apikey.Scope) error {
	if s.config.AccessRepository != nil {
		return accessstore.ErrTransactionRequired
	}
	if scope.credential.Hosted == nil {
		return nil
	}
	identity, membership, err := s.hostedMutationIdentity(ctx, scope.credential)
	if err != nil {
		if !errors.Is(operatorAuthorizationError(err), operatortool.ErrAccessDenied) {
			return operatorAuthorizationError(err)
		}
		return auth.ErrHostedIdentity
	}
	if scope.credential.EntryKey == nil || scope.credential.EntryKey.Kind != "service" {
		var localRole string
		if err := queryRowSQL(ctx, tx, "SELECT role FROM hosted_members WHERE user_id=? AND membership_id=? AND active=1", identity.Subject, membership.ID).Scan(&localRole); err != nil || !auth.ValidOrganizationRole(localRole) {
			return auth.ErrHostedIdentity
		}
		scope.credential.HostedRole = lesserHostedRole(scope.credential.HostedRole, lesserHostedRole(localRole, membership.Role.Slug))
	}
	if !hostedRoleAllows(scope.credential.HostedRole, required, scope.action) {
		return operatortool.ErrAccessDenied
	}
	if scope.requireHostedAdmin && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
		return auth.ErrHostedIdentity
	}
	var count int
	if scope.credential.EntryKey != nil {
		if scope.requireHostedAdmin {
			required = apikey.ScopeAdmin
		}
		if !hostedKeyAllows(scope.credential.HostedKeyScope, required) || !hostedRoleAllows(scope.credential.HostedRole, required, scope.action) {
			return auth.ErrHostedIdentity
		}
		if scope.credential.EntryKey.Kind == "personal" {
			var local string
			err := queryRowSQL(ctx, tx, "SELECT m.role FROM hosted_members m JOIN api_tokens t ON t.id=m.principal_id WHERE m.user_id=? AND m.membership_id=? AND m.active=1 AND t.revoked_at IS NULL", identity.Subject, membership.ID).Scan(&local)
			if err != nil || !auth.ValidOrganizationRole(local) || !hostedRoleAllows(lesserHostedRole(local, membership.Role.Slug), required, scope.action) {
				return auth.ErrHostedIdentity
			}
		}
	} else if scope.credential.HostedKeyScope != "" {
		if scope.requireHostedAdmin {
			required = apikey.ScopeAdmin
		}
		if !hostedKeyAllows(scope.credential.HostedKeyScope, required) {
			return auth.ErrHostedIdentity
		}
		var local, keyScope string
		var access hostedProjectAccess
		err = queryRowSQL(ctx, tx, `SELECT m.role,t.operator_key_scope,t.operator_project_access,m.principal_id FROM hosted_members m JOIN api_tokens t ON t.hosted_user_id=m.user_id AND t.hosted_membership_id=m.membership_id JOIN api_tokens p ON p.id=m.principal_id AND p.revoked_at IS NULL
WHERE t.id=? AND t.token_hash=? AND t.revoked_at IS NULL AND t.native_only=1 AND t.scope IN ('operator','admin') AND t.hosted_organization_id=? AND (t.expires_at IS NULL OR julianday(t.expires_at)>julianday(?)) AND m.user_id=? AND m.membership_id=? AND m.active=1`, scope.credential.ID, scope.credential.Hash, scope.organization, formatHubTime(s.config.now()), identity.Subject, membership.ID).Scan(&local, &keyScope, &access, &scope.credential.HostedPrincipal)
		if err != nil || !auth.ValidOrganizationRole(local) || !hostedRoleAllows(lesserHostedRole(local, membership.Role.Slug), required, scope.action) || !hostedKeyAllows(apikey.Scope(keyScope), required) || access != hostedProjectsAll && access != hostedProjectsSelected {
			return auth.ErrHostedIdentity
		}
		scope.credential.HostedProjectAccess = access
	} else {
		err = queryRowSQL(ctx, tx, `SELECT count(*) FROM hosted_sessions s, hosted_members m
WHERE s.token_hash = ? AND s.revoked_at IS NULL AND julianday(s.expires_at) > julianday(?) AND m.user_id = ? AND m.membership_id = ? AND m.active = 1`, scope.credential.SessionHash, formatHubTime(s.config.now()), identity.Subject, membership.ID).Scan(&count)
		if err != nil || count != 1 {
			return auth.ErrHostedIdentity
		}
	}
	if scope.project != "" {
		if err := s.requireHostedProject(ctx, tx, *scope, required != apikey.ScopeRead); err != nil {
			return err
		}
		condition, args := scope.credential.projectGrantSQL("p.organization_id", "p.id")
		args = append([]any{scope.organization, scope.project}, args...)
		if err := queryRowSQL(ctx, tx, "SELECT count(*) FROM projects p WHERE p.organization_id=? AND p.id=? AND ("+condition+")", args...).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return nativeNotFound()
		}
		if scope.credential.HostedRole == "reporter" && scope.actionItem != "" {
			if err := requireReporterItem(ctx, tx, *scope, scope.actionItem); err != nil {
				return err
			}
			switch scope.action {
			case operatortool.MoveItem:
				issue, _, err := readNativeIssue(ctx, tx, *scope, scope.actionItem)
				if err != nil || scope.actionTarget != "Cancelled" || issue.Actor.PrincipalID != scope.actor().PrincipalID {
					return operatortool.ErrAccessDenied
				}
			case operatortool.VerifyFix:
				issue, _, err := readNativeIssue(ctx, tx, *scope, scope.actionItem)
				if err != nil || issue.Actor.PrincipalID != scope.actor().PrincipalID {
					return operatortool.ErrAccessDenied
				}
			case operatortool.EditComment:
				comment, err := readNativeComment(ctx, tx, *scope, scope.actionItem, scope.actionComment)
				if err != nil || comment.Actor.PrincipalID != scope.actor().PrincipalID {
					return operatortool.ErrAccessDenied
				}
			}
		}
		return nil
	}
	if scope.credential.ManageRunners {
		scope.credential.HostedRole = lesserHostedRole(scope.credential.HostedRole, membership.Role.Slug)
		var projects []tracker.ProjectID
		if scope.managedRunner != "" {
			runner, err := readRunnerWithClock(ctx, tx, scope.organization, scope.managedRunner, s.config.now)
			if err != nil {
				return err
			}
			projects = runnerManagementProjects(runner)
			if scope.managedRunnerScope == "organization" {
				projects = nil
			}
			if projects != nil {
				for _, project := range scope.managedRunnerProjects {
					if !slices.Contains(projects, project) {
						projects = append(projects, project)
					}
				}
			}
		}
		if err := s.requireHostedRunnerProjects(ctx, tx, scope.credential, projects); err != nil {
			return err
		}
		if scope.credential.HostedKeyScope != "" {
			condition, args := scope.credential.projectGrantSQL("p.organization_id", "p.id")
			raw, err := sqlList(projects)
			if err != nil {
				return err
			}
			args = append(args, scope.organization, projects == nil, raw)
			var total, granted int
			err = queryRowSQL(ctx, tx, "SELECT count(*), COALESCE(sum("+condition+"),0) FROM projects p WHERE p.deleted_at IS NULL AND p.organization_id=? AND (? OR p.id IN (SELECT value FROM sql_rows(?)))", args...).Scan(&total, &granted)
			if err != nil || total == 0 || total != granted {
				return auth.ErrHostedIdentity
			}
		}
		return nil
	}
	if operatortool.IsThreadTool(scope.action) {
		return nil
	}
	if membership.Role.Slug != "owner" && membership.Role.Slug != "admin" {
		return auth.ErrHostedIdentity
	}
	return nil
}

func (s *Service) addHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership) error {
	return s.storeHostedMember(ctx, identity, membership, "")
}

func (s *Service) storeHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership, organizationName string) (resultErr error) {
	return s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error { return s.storeAccessMember(ctx, w, identity, membership) })
}

func (s *Service) storeHostedMemberTx(ctx context.Context, tx *sql.Tx, identity auth.Identity, membership auth.Membership, organizationName string) error {
	if !identity.EmailVerified || identity.Subject != membership.UserID || membership.Status != "active" || !auth.ValidOrganizationRole(membership.Role.Slug) {
		return auth.ErrHostedIdentity
	}
	stamp := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, stamp)
	if err != nil {
		return err
	}
	principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
	now := formatHubTime(s.config.now())
	token, err := s.config.generateToken()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens (id,name,token_hash,token_fingerprint,scope,created_at,updated_at,native_only)
VALUES (?,?,?,?, 'operator',?,?,1) ON CONFLICT(id) DO NOTHING`, principal, principal, apikey.HashToken(token), tokenFingerprint(apikey.HashToken(token)), now, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM token_grants WHERE token_id IN (
SELECT principal_id FROM hosted_members WHERE user_id = ? AND (membership_id != ? OR active = 0))`, identity.Subject, membership.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM hosted_project_grants WHERE user_id IN (
SELECT user_id FROM hosted_members WHERE user_id = ? AND (membership_id != ? OR active = 0))`, identity.Subject, membership.ID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO hosted_members(user_id,email,membership_id,role,active,principal_id,created_at,updated_at)
VALUES (?,?,?,?,1,?,?,?) ON CONFLICT(user_id) DO UPDATE SET email=excluded.email,membership_id=excluded.membership_id,role=excluded.role,active=1,updated_at=excluded.updated_at`, identity.Subject, strings.ToLower(identity.Email), membership.ID, membership.Role.Slug, principal, now, now)
	if err != nil {
		return err
	}
	if organizationName != "" {
		if _, err := tx.ExecContext(ctx, "UPDATE organizations SET name = ? WHERE id = ?", organizationName, s.config.Hosted.OrganizationID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_member_reservations WHERE email = ?", strings.ToLower(identity.Email)); err != nil {
		return err
	}
	after, err := s.database.hostedConsumption(ctx, tx, stamp)
	if err != nil {
		return err
	}
	if after["members"] > before["members"] {
		if err := s.database.checkHostedGrowth(ctx, tx, before, stamp, false); err != nil {
			return err
		}
	}
	return nil
}

type hostedExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Service) hostedAudit(ctx context.Context, identity *auth.HostedIdentity, event, route, project string, status int) error {
	return s.recordAccessAudit(ctx, identity, event, route, project, status)
}

func (s *Service) hostedAuditWith(ctx context.Context, exec hostedExecer, identity *auth.HostedIdentity, event, route, project string, status int) error {
	if s.config.AccessRepository != nil {
		return accessstore.ErrTransactionRequired
	}
	if identity == nil {
		return nil
	}
	if audit := entryKeyAuditFrom(ctx); audit != nil && (event == "action" || event == "administration") {
		if _, transactional := exec.(*sql.Tx); transactional {
			return s.auditEntryKey(ctx, exec, audit, status)
		}
		return nil
	}
	if _, ok := mutation.FromContext(ctx); !ok && status < http.StatusBadRequest {
		switch event {
		case "billing_viewed", "billing_exported":
			if identity.SupportActor == "" {
				return nil
			}
		case "action", "administration":
			method, _, _ := strings.Cut(route, " ")
			switch method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				if identity.SupportActor == "" {
					return nil
				}
			}
		}
	}
	actor := identity.Subject
	if identity.SupportActor != "" {
		actor = identity.SupportActor
	}
	summary := "{}"
	if m, ok := mutation.FromContext(ctx); ok {
		if m.ActorKind != "" && m.ActorKind != "human" {
			actor = m.PrincipalID
		}
		m.PrincipalID, m.OrganizationID = actor, s.config.Hosted.OrganizationID
		m.RetryIdentity, m.InputHash = "", ""
		raw, err := json.Marshal(struct {
			mutation.Metadata
			Outcome string `json:"outcome"`
		}{m, event})
		if err != nil {
			return err
		}
		summary = string(raw)
	}
	now := s.config.now()
	account := decodeAccountFields(event)
	_, err := exec.ExecContext(ctx, `INSERT INTO hosted_audit(organization_id,session_id,actual_actor,effective_user,reason,event,route,project_id,status,started_at,expires_at,recorded_at,mutation_json,recorded_at_ns,account_kind,account_retry)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.config.Hosted.OrganizationID, identity.SessionID, actor, identity.Subject, identity.SupportReason, event, route, project, status, formatHubTime(identity.CreatedAt), formatHubTime(identity.ExpiresAt), formatHubTime(now), summary, now.UnixNano(), account.Kind, account.Retry)
	return err
}
