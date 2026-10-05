package cloudentry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

const staleEntitlementMessage = "Someone changed this organization's plan; reload and try again."

type planReference struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type entitlementPlan struct {
	planReference
	Features   []string         `json:"features"`
	Allowances map[string]int64 `json:"allowances"`
}

type entitlementGrant struct {
	ID        string        `json:"id"`
	Plan      planReference `json:"plan"`
	Scope     []string      `json:"scope"`
	StartsAt  time.Time     `json:"starts_at"`
	ExpiresAt *time.Time    `json:"expires_at"`
	RevokedAt *time.Time    `json:"revoked_at,omitempty"`
	Reason    string        `json:"reason"`
	GrantedBy string        `json:"granted_by"`
	GrantedAt *time.Time    `json:"granted_at"`
}

type organizationEntitlements struct {
	OrganizationID string             `json:"organization_id"`
	Base           planReference      `json:"base"`
	EffectiveBase  planReference      `json:"effective_base"`
	Source         string             `json:"source"`
	Revision       int64              `json:"revision"`
	Features       []string           `json:"features"`
	Grants         []entitlementGrant `json:"grants"`
	Plans          []entitlementPlan  `json:"plans"`
}

type entitlementChange struct {
	Action           string        `json:"action"`
	Feature          string        `json:"feature,omitempty"`
	IdempotencyKey   string        `json:"idempotency_key"`
	ExpectedRevision int64         `json:"expected_revision"`
	Plan             planReference `json:"plan"`
	GrantID          string        `json:"grant_id"`
	ExpiresAt        *time.Time    `json:"expires_at"`
	Reason           string        `json:"reason"`
}

type tenantPlanCommand struct {
	ID               string        `json:"idempotency_key"`
	Action           string        `json:"action"`
	ExpectedRevision int64         `json:"expected_revision"`
	Plan             planReference `json:"plan"`
	GrantID          string        `json:"grant_id"`
	Scope            []string      `json:"scope"`
	ExpiresAt        *time.Time    `json:"expires_at"`
	Reason           string        `json:"reason"`
}

type tenantStatusError struct{ status int }

func (e *tenantStatusError) Error() string {
	return fmt.Sprintf("tenant entitlements answered %d", e.status)
}

func (c Config) validateEntitlementAdministrators() error {
	for _, email := range c.EntitlementAdministrators {
		if !listed(c.StaffEmails, email) {
			return fmt.Errorf("entitlement administrator %q must also be listed in staff_emails", email)
		}
	}
	if len(c.EntitlementAdministrators) > 0 && (c.Allocation == nil || len(c.Allocation.EntitlementAdminToken) < 32) {
		return errors.New("entitlement_administrators requires allocation.entitlement_admin_token_env naming a token of at least 32 bytes")
	}
	return nil
}

func (s *Service) entitlementAdministrator(session accountSession) bool {
	return s.platformStaff(session) && listed(s.config.EntitlementAdministrators, session.Email) && s.config.Allocation != nil && len(s.config.Allocation.EntitlementAdminToken) >= 32
}

func tenantEntitlementsPath(organization string) string {
	return "/api/v2/organizations/" + organization + "/entitlements"
}

func (s *Service) tenantEntitlements(ctx context.Context, organization Organization) (organizationEntitlements, error) {
	var result organizationEntitlements
	status, body, err := s.machineCall(ctx, organization, http.MethodGet, tenantEntitlementsPath(organization.ID), nil, string(s.config.Allocation.EntitlementAdminToken))
	if err != nil {
		return result, err
	}
	if status != http.StatusOK {
		return result, &tenantStatusError{status}
	}
	if err := json.Unmarshal(body, &result); err != nil || result.OrganizationID != organization.ID {
		return result, errors.Join(errors.New("tenant entitlements are malformed"), err)
	}
	granters, err := s.registry.entitlementGranters(ctx, organization.ID)
	if err != nil {
		return result, err
	}
	for index, grant := range result.Grants {
		if email, ok := granters[grant.ID]; ok {
			result.Grants[index].GrantedBy = email
		}
	}
	return result, nil
}

func activeGrants(grants []entitlementGrant, now time.Time) []entitlementGrant {
	active := []entitlementGrant{}
	for _, grant := range grants {
		if grant.RevokedAt == nil && !grant.StartsAt.After(now) && (grant.ExpiresAt == nil || grant.ExpiresAt.After(now)) {
			active = append(active, grant)
		}
	}
	return active
}

func (r *Registry) entitlementGranters(ctx context.Context, organization string) (map[string]string, error) {
	rows, err := r.store.db.QueryContext(ctx, "SELECT grant_id,staff_email FROM entitlement_changes WHERE organization_id = ? AND action = 'grant'", organization)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]string)
	for rows.Next() {
		var grant, email string
		if err := rows.Scan(&grant, &email); err != nil {
			return nil, err
		}
		result[grant] = email
	}
	return result, rows.Err()
}

func (s *Service) entitlementSession(c echo.Context, event string) (accountSession, Organization, bool, error) {
	session, err := s.session(c)
	if err != nil {
		return accountSession{}, Organization{}, false, c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	if !s.entitlementAdministrator(session) {
		return accountSession{}, Organization{}, false, c.JSON(http.StatusForbidden, map[string]string{"code": "forbidden", "message": "Complimentary plans are limited to entitlement administrators"})
	}
	ctx := c.Request().Context()
	organization, err := s.readyOrganization(ctx, c.Param("organization"))
	if err != nil {
		return accountSession{}, Organization{}, false, c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "That organization is not ready"})
	}
	if err := s.auth.audit(ctx, session.Subject, organization.ID, event); err != nil {
		return accountSession{}, Organization{}, false, c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "The platform console is temporarily unavailable"})
	}
	return session, organization, true, nil
}

func (s *Service) tenantEntitlementFailure(c echo.Context, err error) error {
	var failure *tenantStatusError
	if errors.As(err, &failure) {
		switch failure.status {
		case http.StatusConflict:
			return c.JSON(http.StatusConflict, map[string]string{"code": "revision_conflict", "message": staleEntitlementMessage})
		case http.StatusNotFound:
			return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "That grant or plan no longer exists on the organization's Hub"})
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "The organization's Hub rejected this plan change"})
		case http.StatusForbidden, http.StatusUnauthorized:
			return c.JSON(http.StatusBadGateway, map[string]string{"code": "entitlement_credential_rejected", "message": "The organization's Hub does not accept the configured entitlement credential"})
		}
	}
	s.config.Logger.Warn("tenant entitlements unavailable", "error", err)
	return c.JSON(http.StatusBadGateway, map[string]string{"code": "tenant_unavailable", "message": "The organization's Hub is temporarily unavailable"})
}

func (s *Service) platformEntitlementsJSON(c echo.Context) error {
	_, organization, allowed, err := s.entitlementSession(c, "platform_entitlements_viewed")
	if !allowed {
		return err
	}
	result, err := s.tenantEntitlements(c.Request().Context(), organization)
	if err != nil {
		return s.tenantEntitlementFailure(c, err)
	}
	result.Grants = activeGrants(result.Grants, s.config.now())
	return c.JSON(http.StatusOK, result)
}

func complimentaryGrantID(organization, key string) string {
	digest := sha256.Sum256([]byte(organization + "\x00" + key))
	return "comp_" + hex.EncodeToString(digest[:10])
}

func planScope(plan entitlementPlan) []string {
	scope := slices.Clone(plan.Features)
	for name, limit := range plan.Allowances {
		if limit > 0 {
			scope = append(scope, name)
		}
	}
	slices.Sort(scope)
	return slices.Compact(scope)
}

func (s *Service) changePlatformEntitlement(c echo.Context) error {
	session, organization, allowed, err := s.entitlementSession(c, "platform_entitlement_change_requested")
	if !allowed {
		return err
	}
	if !s.csrfValid(c, session, "") {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "csrf_invalid", "message": "Reload the page and try again"})
	}
	var change entitlementChange
	decoder := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"code": "invalid_request", "message": "The plan change could not be read"})
	}
	change.Reason = strings.TrimSpace(change.Reason)
	switch {
	case change.Action != "grant" && change.Action != "revoke":
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "Choose grant or revoke"})
	case change.Feature != "" && (change.Action != "grant" || change.Feature != "model_choice" || change.Plan != (planReference{})):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "Choose model_choice without a plan for a feature grant"})
	case !safeID(change.IdempotencyKey) || change.ExpectedRevision < 1:
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "Reload the organization's plan and try again"})
	case change.Reason == "" || len(change.Reason) > 500:
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "reason_required", "message": "Give a reason of at most 500 characters"})
	case change.Action == "grant" && change.ExpiresAt != nil && !change.ExpiresAt.After(s.config.now()):
		return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "The expiry must be in the future"})
	}
	ctx := c.Request().Context()
	current, err := s.tenantEntitlements(ctx, organization)
	if err != nil {
		return s.tenantEntitlementFailure(c, err)
	}
	command := tenantPlanCommand{ID: change.IdempotencyKey, Action: change.Action, ExpectedRevision: change.ExpectedRevision, Reason: change.Reason}
	switch change.Action {
	case "grant":
		if change.Feature == "model_choice" {
			command.Plan, command.Scope = current.Base, []string{change.Feature}
		} else {
			index := slices.IndexFunc(current.Plans, func(plan entitlementPlan) bool { return plan.planReference == change.Plan })
			if index < 0 {
				return c.JSON(http.StatusUnprocessableEntity, map[string]string{"code": "invalid_request", "message": "Choose a configured plan"})
			}
			command.Plan, command.Scope = change.Plan, planScope(current.Plans[index])
		}
		command.ExpiresAt = change.ExpiresAt
		command.GrantID = complimentaryGrantID(organization.ID, change.IdempotencyKey)
		if command.ExpiresAt != nil {
			expiry := command.ExpiresAt.UTC()
			command.ExpiresAt = &expiry
		}
	case "revoke":
		index := slices.IndexFunc(current.Grants, func(grant entitlementGrant) bool { return grant.ID == change.GrantID })
		if index < 0 {
			return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "That grant no longer exists on the organization's Hub"})
		}
		command.GrantID = change.GrantID
		command.Plan, command.ExpiresAt = current.Grants[index].Plan, current.Grants[index].ExpiresAt
	}
	body, err := json.Marshal(command)
	if err != nil {
		return err
	}
	status, _, err := s.machineCall(ctx, organization, http.MethodPost, tenantEntitlementsPath(organization.ID), body, string(s.config.Allocation.EntitlementAdminToken))
	if err == nil && status != http.StatusNoContent {
		err = &tenantStatusError{status}
	}
	if err != nil {
		return s.tenantEntitlementFailure(c, err)
	}
	// The tenant has applied the change, so the staff record is written even
	// if the browser has gone; a retry with the same key replays it.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.registry.recordEntitlementChange(recordCtx, organization.ID, session, command, s.config.now()); err != nil {
		s.config.Logger.Error("entitlement change applied but not recorded in the entry audit log", "organization", organization.ID, "idempotency_key", command.ID)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "audit_unavailable", "message": "The change was applied but could not be recorded; retry to record it"})
	}
	s.config.Logger.Info("complimentary plan changed", "organization", organization.ID, "action", command.Action, "plan", command.Plan.ID, "grant", command.GrantID, "staff", session.Email)
	return c.JSON(http.StatusOK, map[string]string{"action": command.Action, "grant_id": command.GrantID})
}

func (r *Registry) recordEntitlementChange(ctx context.Context, organization string, session accountSession, command tenantPlanCommand, now time.Time) error {
	expires := ""
	if command.ExpiresAt != nil {
		expires = formatTime(*command.ExpiresAt)
	}
	_, err := r.store.db.ExecContext(ctx, "INSERT INTO entitlement_changes(organization_id,idempotency_key,action,grant_id,plan_id,plan_version,expires_at,reason,staff_email,staff_subject,recorded_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,idempotency_key) DO NOTHING",
		organization, command.ID, command.Action, command.GrantID, command.Plan.ID, command.Plan.Version, expires, command.Reason, strings.ToLower(session.Email), session.Subject, formatTime(now))
	return err
}
