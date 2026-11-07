package cloudentry

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type keyOrganization struct {
	apikey.ProjectContext
	OrganizationID string `json:"organization_id"`
}

type accessKey struct {
	OAuth               bool              `json:"-"`
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	Kind                string            `json:"kind"`
	Owner               string            `json:"owner"`
	OwnerEmail          string            `json:"owner_email"`
	Permission          apikey.Scope      `json:"permission"`
	AccessContext       string            `json:"access_context"`
	Organizations       []keyOrganization `json:"organizations"`
	ServiceOrganization string            `json:"service_organization_id,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	RevokedAt           *time.Time        `json:"revoked_at,omitempty"`
	Token               string            `json:"token,omitempty"`
	Hash                string            `json:"-"`
}

func (k accessKey) projectContext(organization string) (apikey.ProjectContext, bool) {
	for _, entry := range k.Organizations {
		if entry.OrganizationID == organization {
			return entry.ProjectContext, true
		}
	}
	return apikey.ProjectContext{Access: "all"}, k.AccessContext == "global" && k.Kind == "personal"
}

func (k accessKey) valid() bool {
	if k.Kind != "personal" && k.Kind != "service" || !apikey.ValidScope(k.Permission) || len(k.Organizations) > 200 || k.Owner == "" || k.Name == "" || len(k.Name) > 128 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range k.Organizations {
		if !safeID(item.OrganizationID) || !item.Valid() || seen[item.OrganizationID] {
			return false
		}
		seen[item.OrganizationID] = true
	}
	if k.Kind == "service" && (len(k.Organizations) != 1 || k.Organizations[0].OrganizationID != k.ServiceOrganization) {
		return false
	}
	switch k.AccessContext {
	case "global":
		return k.Kind == "personal"
	case "selected":
		return len(k.Organizations) > 0
	case "project":
		return len(k.Organizations) == 1 && k.Organizations[0].Access == "project"
	default:
		return false
	}
}

func (a *authStore) accessKey(ctx context.Context, column, value string) (accessKey, error) {
	return a.postgresKey(ctx, column, value)
}

func (s *Service) activeAccessKey(ctx context.Context, hash string) (accessKey, error) {
	k, err := s.auth.accessKey(ctx, "token_hash", hash)
	if err != nil {
		return k, err
	}
	if !k.active(s.config.now()) {
		return k, &apikey.Refusal{Code: "key_inactive", Message: "The key is expired or revoked"}
	}
	return k, nil
}

func (k accessKey) active(now time.Time) bool {
	return k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now))
}

func (k accessKey) credentialBinding() string {
	sum := sha256.Sum256([]byte("detent-entry-coordinator-credential-v1\x00" + k.Hash))
	return hex.EncodeToString(sum[:])
}

func (s *Service) keyMembership(ctx context.Context, owner string, organization Organization) (auth.Membership, error) {
	items, err := s.config.Provider.Memberships(ctx, owner, organization.ProviderID)
	if err != nil {
		return auth.Membership{}, fmt.Errorf("read current key owner membership: %w", errors.Join(tenantauth.ErrUnavailable, err))
	}
	for _, item := range items {
		if item.UserID == owner && item.OrganizationID == organization.ProviderID && item.Status == "active" && auth.ValidOrganizationRole(item.Role.Slug) {
			return item, nil
		}
	}
	return auth.Membership{}, &apikey.Refusal{Code: "membership_denied", Message: "The key owner is not a current organization member"}
}

func (s *Service) keyClaims(ctx context.Context, k accessKey, organization Organization, method, path string, body []byte) (cloudassert.Claims, error) {
	return s.prepareKeyClaims(ctx, k, organization, method, path, body, false)
}

func (s *Service) prepareKeyClaims(ctx context.Context, k accessKey, organization Organization, method, path string, body []byte, recordUse bool) (result cloudassert.Claims, resultErr error) {
	defer s.auditKeyRefusal(ctx, organization, k.ID, k.Owner, &resultErr)
	if _, reaches := k.projectContext(organization.ID); !reaches {
		return cloudassert.Claims{}, &apikey.Refusal{Code: "key_organization_denied", Message: "The organization is outside this key's access context"}
	}
	role, subject := "admin", "service:"+k.ID
	if k.Kind == "personal" {
		member, err := s.keyMembership(ctx, k.Owner, organization)
		if err != nil {
			return cloudassert.Claims{}, err
		}
		role, subject = member.Role.Slug, k.Owner
	}
	snapshot, err := s.auth.store.db.postgres.PlatformKey(ctx, tenantauth.PlatformKeyRequest{Organization: tracker.OrganizationID(organization.ID), KeyID: k.ID, OAuthExchange: k.OAuth && k.ExpiresAt == nil})
	if err != nil {
		return cloudassert.Claims{}, err
	}
	if snapshot.Generation != organization.Generation || snapshot.ProviderID != organization.ProviderID || snapshot.Endpoint != organization.Endpoint || snapshot.OwnerSubject != k.Owner || snapshot.Authority.Kind != k.Kind || subtle.ConstantTimeCompare([]byte(snapshot.Authority.CredentialBinding), []byte(k.credentialBinding())) != 1 || k.OAuth && snapshot.Resource != s.config.PublicURL+"/mcp" {
		return cloudassert.Claims{}, errors.Join(tenantauth.ErrDenied, &apikey.Refusal{Code: "access_denied", Message: "The key authority changed"})
	}
	if recordUse {
		if err := s.recordCredentialUse(ctx, k, organization.ID); err != nil {
			return cloudassert.Claims{}, err
		}
	}
	claims, err := s.claims(organization, cloudassert.KindKey, method, path, body)
	if err != nil {
		return claims, err
	}
	claims.Key, claims.Subject, claims.Email, claims.ProviderOrganization, claims.Role = &snapshot.Authority, subject, snapshot.OwnerEmail, snapshot.ProviderID, role
	if snapshot.ExpiresAt != nil && snapshot.ExpiresAt.Before(claims.ExpiresAt) {
		claims.ExpiresAt = *snapshot.ExpiresAt
	}
	return claims, nil
}

func (s *Service) auditKeyRefusal(ctx context.Context, organization Organization, key, owner string, resultErr *error) {
	var refusal *apikey.Refusal
	event := "access_denied"
	if errors.As(*resultErr, &refusal) {
		event = refusal.Code
	} else if !errors.Is(*resultErr, cloudassert.ErrInvalid) && !errors.Is(*resultErr, tenantauth.ErrDenied) {
		return
	}
	if !safeID(key) {
		key = ""
	}
	if owner == "" || len(owner) > 256 {
		owner = "unknown"
	}
	if err := s.keyAudit(ctx, organization, key, owner, event); err != nil {
		*resultErr = fmt.Errorf("audit key refusal: %w", errors.Join(tenantauth.ErrUnavailable, err))
	}
}

func (s *Service) recordKeyUse(ctx context.Context, key, organization string) error {
	now := formatTime(s.config.now())
	_, err := s.auth.store.db.ExecContext(ctx, `INSERT INTO platform.access_key_organizations(key_id,organization_id,reached_at,last_used_at) VALUES($1,$2,$3,$4) ON CONFLICT(key_id,organization_id) DO UPDATE SET reached_at=COALESCE(platform.access_key_organizations.reached_at,excluded.reached_at),last_used_at=excluded.last_used_at`, key, organization, now, now)
	return err
}

func keyBearer(c echo.Context) (string, error) {
	values := c.Request().Header.Values(echo.HeaderAuthorization)
	if len(values) != 1 {
		return "", &apikey.Refusal{Code: "key_required", Message: "One bearer key is required"}
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", &apikey.Refusal{Code: "key_required", Message: "A valid bearer key is required"}
	}
	return apikey.HashToken(token), nil
}

func keyError(c echo.Context, err error) error {
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return c.JSON(http.StatusServiceUnavailable, apikey.Refusal{Code: "key_authority_unavailable", Message: "Key authorization is temporarily unavailable"})
	}
	var pending *tenantPendingError
	if errors.As(err, &pending) {
		return c.JSON(pending.status, &pending.refusal)
	}
	var refusal *apikey.Refusal
	if errors.As(err, &refusal) {
		return c.JSON(http.StatusForbidden, refusal)
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errNoSession) {
		return c.JSON(http.StatusUnauthorized, apikey.Refusal{Code: "key_required", Message: "A valid key is required"})
	}
	if errors.Is(err, tenantauth.ErrDenied) || errors.Is(err, cloudassert.ErrInvalid) {
		return c.JSON(http.StatusForbidden, apikey.Refusal{Code: "access_denied", Message: "Key access is denied"})
	}
	return c.JSON(http.StatusServiceUnavailable, apikey.Refusal{Code: "key_authority_unavailable", Message: "Key authorization is temporarily unavailable"})
}

func (s *Service) accountKeySession(c echo.Context) (accountSession, error) {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		return accountSession{}, errNoSession
	}
	if c.Request().Method != http.MethodGet && !s.sameOrigin(c) {
		return accountSession{}, errNoSession
	}
	session, err := s.session(c)
	if err != nil {
		return accountSession{}, err
	}
	if session.Identity.SupportActor != "" {
		return accountSession{}, errNoSession
	}
	return session, nil
}

func (s *Service) registerAccessKeys() {
	s.echo.GET("/api/cloud/account/api-keys", s.accountKeys)
	s.echo.POST("/api/cloud/account/api-keys", s.createAccountKey)
	s.echo.POST("/api/cloud/account/api-keys/:key/rotate", s.rotateAccountKey)
	s.echo.GET("/api/cloud/account/key-context", s.accountKeyContext)
	s.echo.DELETE("/api/cloud/account/api-keys/:key", s.revokeAccountKey)
	s.echo.GET("/api/cloud/account/key-notifications", s.keyNotifications)
	s.echo.GET("/api/cloud/organizations/:organization/external-keys", s.organizationKeys)
	s.echo.GET("/api/cloud/organizations/:organization/key-policy", s.organizationKeyPolicy)
	s.echo.PUT("/api/cloud/organizations/:organization/key-policy", s.organizationKeyPolicy)
	s.echo.PUT("/api/cloud/organizations/:organization/external-keys/:key/block", s.blockOrganizationKey)
	s.echo.PUT("/api/cloud/organizations/:organization/external-keys/:key/approve", s.approveOrganizationKey)
	s.echo.POST("/api/cloud/organizations/:organization/service-keys", s.createAccountKey)
	s.echo.GET("/api/cloud/organizations/:organization/service-keys", s.serviceKeys)
	s.echo.DELETE("/api/cloud/organizations/:organization/service-keys/:key", s.revokeServiceKey)
}

func (s *Service) createAccountKey(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	var input struct {
		Name          string            `json:"name"`
		Permission    apikey.Scope      `json:"permission"`
		AccessContext string            `json:"access_context"`
		Organizations []keyOrganization `json:"organizations"`
		ExpiresDays   int               `json:"expires_days"`
		NeverExpires  bool              `json:"never_expires"`
	}
	if err := decodeKeyJSON(c, &input); err != nil {
		return err
	}
	k := accessKey{Name: strings.TrimSpace(input.Name), Kind: "personal", Owner: session.Subject, OwnerEmail: session.Email, Permission: input.Permission, AccessContext: input.AccessContext, Organizations: input.Organizations, CreatedAt: s.config.now()}
	if c.Param("organization") != "" {
		k.Kind, k.ServiceOrganization = "service", c.Param("organization")
		if _, err := s.keyAdmin(c, session); err != nil {
			return keyError(c, err)
		}
	}
	if !k.valid() || input.NeverExpires && input.ExpiresDays != 0 || !input.NeverExpires && (input.ExpiresDays < 1 || input.ExpiresDays > 90) {
		return c.JSON(http.StatusUnprocessableEntity, apikey.Refusal{Code: "invalid_key_context", Message: "Choose a valid key context, permission and Never or 1–90 days"})
	}
	if !input.NeverExpires {
		expiry := s.config.now().Add(time.Duration(input.ExpiresDays) * 24 * time.Hour)
		k.ExpiresAt = &expiry
	}
	for _, item := range k.Organizations {
		org, err := s.readyOrganization(c.Request().Context(), item.OrganizationID)
		if err != nil {
			return keyError(c, err)
		}
		if _, err := s.keyMembership(c.Request().Context(), session.Subject, org); err != nil {
			return keyError(c, err)
		}
	}
	k.ID, err = cloudassert.NewID()
	if err != nil {
		return keyError(c, err)
	}
	k.ID = "key_" + k.ID
	k.Token, err = s.config.generateToken()
	if err != nil {
		return keyError(c, err)
	}
	k.Hash = apikey.HashToken(k.Token)
	err = s.auth.postgresCreateKey(c.Request().Context(), k)
	if err != nil {
		return keyError(c, err)
	}
	return c.JSON(http.StatusCreated, k)
}

func decodeKeyJSON(c echo.Context, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid key request")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest, "Expected one key request")
	}
	return nil
}

func (s *Service) accountKeys(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	keys, err := s.listAccessKeys(c.Request().Context(), "SELECT id FROM platform.access_keys WHERE owner_subject=$1 AND kind='personal' ORDER BY created_at DESC,id LIMIT 200", session.Subject)
	if err != nil {
		return keyError(c, err)
	}
	organizations, err := s.organizationChoices(c.Request().Context(), session)
	if err != nil {
		return keyError(c, err)
	}
	orgs := []Organization{}
	for _, choice := range organizations {
		org, err := s.readyOrganization(c.Request().Context(), choice.ID)
		if err != nil {
			return keyError(c, err)
		}
		orgs = append(orgs, org)
	}
	views := []keyView{}
	reachCache := map[keyReachOwner]keyReach{}
	for _, key := range keys {
		view, err := s.describeKeyWithReachCache(c.Request().Context(), key, orgs, reachCache)
		if err != nil {
			return keyError(c, err)
		}
		views = append(views, view)
	}
	return c.JSON(http.StatusOK, struct {
		Keys []keyView `json:"keys"`
	}{views})
}

func (s *Service) listAccessKeys(ctx context.Context, query string, args ...any) (_ []accessKey, resultErr error) {
	rows, err := s.auth.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer closeEntryRows(rows, &resultErr)
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	keys := make([]accessKey, 0, len(ids))
	for _, id := range ids {
		k, err := s.auth.accessKey(ctx, "id", id)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Service) revokeAccountKey(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	result, err := s.auth.store.db.ExecContext(c.Request().Context(), "UPDATE platform.access_keys SET revoked_at=COALESCE(revoked_at,$1) WHERE id=$2 AND owner_subject=$3 AND kind='personal'", formatTime(s.config.now()), c.Param("key"), session.Subject)
	if err != nil {
		return keyError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return keyError(c, err)
	}
	if count != 1 {
		return c.NoContent(http.StatusNotFound)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) keyAdmin(c echo.Context, session accountSession) (Organization, error) {
	org, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return org, err
	}
	member, err := s.keyMembership(c.Request().Context(), session.Subject, org)
	if err != nil {
		return org, err
	}
	if member.Role.Slug != "owner" && member.Role.Slug != "admin" {
		return org, &apikey.Refusal{Code: "role_denied", Message: "Organization owner or administrator role is required"}
	}
	status, _, err := s.serviceCall(c.Request().Context(), org, "/internal/v1/keys/admin", struct{}{}, func(claims *cloudassert.Claims) {
		claims.Subject, claims.Email, claims.Role = session.Subject, session.Email, member.Role.Slug
	})
	if err != nil {
		return org, err
	}
	if status != http.StatusOK && status != http.StatusUnauthorized && status != http.StatusForbidden {
		return org, tenantauth.ErrUnavailable
	}
	if status != http.StatusOK {
		return org, &apikey.Refusal{Code: "role_denied", Message: "Current organization administrator authority is required"}
	}
	return org, nil
}

func (s *Service) serviceKeys(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	keys, err := s.listAccessKeys(c.Request().Context(), "SELECT id FROM platform.access_keys WHERE kind='service' AND service_organization_id=$1 ORDER BY created_at DESC,id LIMIT 200", org.ID)
	if err != nil {
		return keyError(c, err)
	}
	type serviceKeyView struct {
		accessKey
		LastUsedAt *string `json:"last_used_at"`
	}
	views := []serviceKeyView{}
	for _, key := range keys {
		var last sql.NullString
		if err := s.auth.store.db.QueryRowContext(c.Request().Context(), "SELECT MAX(last_used_at) FROM platform.access_key_organizations WHERE key_id=$1 AND organization_id=$2", key.ID, org.ID).Scan(&last); err != nil {
			return keyError(c, err)
		}
		view := serviceKeyView{accessKey: key}
		if last.Valid {
			view.LastUsedAt = &last.String
		}
		views = append(views, view)
	}
	return c.JSON(http.StatusOK, struct {
		Keys []serviceKeyView `json:"keys"`
	}{views})
}

func (s *Service) revokeServiceKey(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	result, err := s.auth.store.db.ExecContext(c.Request().Context(), "UPDATE platform.access_keys SET revoked_at=COALESCE(revoked_at,$1) WHERE id=$2 AND kind='service' AND service_organization_id=$3", formatTime(s.config.now()), c.Param("key"), org.ID)
	if err != nil {
		return keyError(c, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return keyError(c, err)
	}
	if count != 1 {
		return c.NoContent(http.StatusNotFound)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) organizationKeyPolicy(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	policy, err := s.personalKeyPolicy(c.Request().Context(), org.ID)
	if err != nil {
		return keyError(c, err)
	}
	input := struct {
		Policy  string `json:"personal_keys"`
		Allowed *bool  `json:"allow_external_keys,omitempty"`
	}{Policy: policy}
	if c.Request().Method == http.MethodPut {
		if err := decodeKeyJSON(c, &input); err != nil {
			return err
		}
		if input.Allowed != nil {
			input.Policy = "allowed"
			if !*input.Allowed {
				input.Policy = "blocked"
			}
		}
		if input.Policy != "allowed" && input.Policy != "approval" && input.Policy != "blocked" {
			return c.NoContent(http.StatusUnprocessableEntity)
		}
		_, err = s.auth.store.db.ExecContext(c.Request().Context(), "INSERT INTO platform.organization_key_policy(organization_id,personal_keys) VALUES($1,$2) ON CONFLICT(organization_id) DO UPDATE SET personal_keys=excluded.personal_keys", org.ID, input.Policy)
		if err != nil {
			return keyError(c, err)
		}
		if err := s.keyAudit(c.Request().Context(), org, "", session.Subject, "external_key_policy_changed"); err != nil {
			return keyError(c, err)
		}
	}
	return c.JSON(http.StatusOK, input)
}

func (s *Service) organizationKeys(c echo.Context) error {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	ctx := c.Request().Context()
	cursor := c.QueryParam("cursor")
	if len(cursor) > 256 {
		return c.NoContent(http.StatusBadRequest)
	}
	memberships, err := s.config.Provider.Memberships(ctx, "", org.ProviderID)
	if err != nil {
		return keyError(c, err)
	}
	owners := []string{}
	for _, member := range memberships {
		if member.OrganizationID == org.ProviderID && member.Status == "active" && auth.ValidOrganizationRole(member.Role.Slug) {
			owners = append(owners, member.UserID)
		}
	}
	keys, err := s.listAccessKeys(ctx, `SELECT id FROM platform.access_keys WHERE owner_subject = ANY($1) AND kind='personal' AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at>$2) AND id>$3 AND (access_context='global' OR EXISTS(SELECT 1 FROM platform.access_key_organizations o WHERE o.key_id=platform.access_keys.id AND o.organization_id=$4 AND o.selected)) ORDER BY id LIMIT 51`, owners, formatTime(s.config.now()), cursor, org.ID)
	if err != nil {
		return keyError(c, err)
	}
	nextCursor := ""
	if len(keys) > 50 {
		keys = keys[:50]
		nextCursor = keys[len(keys)-1].ID
	}
	reachCache := map[keyReachOwner]keyReach{}
	result := []keyView{}
	for _, key := range keys {
		if _, reaches := key.projectContext(org.ID); !reaches {
			continue
		}
		view, err := s.describeKeyWithReachCache(ctx, key, []Organization{org}, reachCache)
		if err != nil {
			return keyError(c, err)
		}
		if len(view.EffectiveReach) == 0 {
			continue
		}
		view.LastUsedAt = view.EffectiveReach[0].LastUsedAt
		view.Organizations = slices.DeleteFunc(slices.Clone(view.Organizations), func(item keyOrganization) bool { return item.OrganizationID != org.ID })
		view.ReadOnly = true
		result = append(result, view)
	}
	return c.JSON(http.StatusOK, struct {
		Keys       []keyView `json:"keys"`
		NextCursor string    `json:"next_cursor,omitempty"`
	}{result, nextCursor})
}

func (s *Service) blockOrganizationKey(c echo.Context) (resultErr error) {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	org, err := s.keyAdmin(c, session)
	if err != nil {
		return keyError(c, err)
	}
	k, err := s.auth.accessKey(c.Request().Context(), "id", c.Param("key"))
	if err != nil {
		return keyError(c, err)
	}
	if _, reaches := k.projectContext(org.ID); !reaches || k.Kind != "personal" {
		return c.NoContent(http.StatusNotFound)
	}
	var input struct {
		ProjectID string `json:"project_id"`
	}
	if err := decodeKeyJSON(c, &input); err != nil {
		return err
	}
	if len(input.ProjectID) > 256 {
		return c.NoContent(http.StatusBadRequest)
	}
	if err := s.auth.store.db.postgres.BlockKey(c.Request().Context(), tracker.OrganizationID(org.ID), k.ID, tracker.ProjectID(input.ProjectID)); err != nil {
		return keyError(c, err)
	}
	if err := s.keyAudit(c.Request().Context(), org, k.ID, session.Subject, "personal_key_blocked"); err != nil {
		return keyError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) keyNotifications(c echo.Context) (resultErr error) {
	session, err := s.accountKeySession(c)
	if err != nil {
		return keyError(c, err)
	}
	rows, err := s.auth.store.db.QueryContext(c.Request().Context(), "SELECT key_id,organization_id,project_id,created_at FROM platform.access_key_notifications WHERE owner_subject=$1 ORDER BY id DESC LIMIT 200", session.Subject)
	if err != nil {
		return keyError(c, err)
	}
	defer closeEntryRows(rows, &resultErr)
	type notice struct {
		KeyID          string `json:"key_id"`
		OrganizationID string `json:"organization_id"`
		ProjectID      string `json:"project_id,omitempty"`
		CreatedAt      string `json:"created_at"`
	}
	result := []notice{}
	for rows.Next() {
		var item notice
		if err := rows.Scan(&item.KeyID, &item.OrganizationID, &item.ProjectID, &item.CreatedAt); err != nil {
			return keyError(c, err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return keyError(c, err)
	}
	return c.JSON(http.StatusOK, struct {
		Notifications []notice `json:"notifications"`
	}{result})
}

func (s *Service) keyAudit(ctx context.Context, org Organization, key, owner, event string) error {
	status, _, err := s.serviceCall(ctx, org, "/internal/v1/keys/audit", struct {
		KeyID string `json:"key_id"`
		Owner string `json:"owner"`
		Event string `json:"event"`
	}{key, owner, event}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return errors.New("target organization key audit unavailable")
	}
	return nil
}
