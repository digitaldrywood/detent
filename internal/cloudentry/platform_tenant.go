package cloudentry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

type platformTenantMember struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type platformRunner struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Health string `json:"health"`
}

type platformProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type platformOrganizationEvent struct {
	Event      string `json:"event"`
	Generation int64  `json:"generation"`
	RecordedAt string `json:"recorded_at"`
}

type platformTenantSections struct {
	Members      *[]platformTenantMember   `json:"members"`
	Runners      *[]platformRunner         `json:"runners"`
	Projects     *[]platformProject        `json:"projects"`
	Entitlements *organizationEntitlements `json:"entitlements"`
}

type platformTenantDetail struct {
	platformTenantSections
	Organization platformOrganization        `json:"organization"`
	Events       []platformOrganizationEvent `json:"events"`
	Unavailable  []string                    `json:"unavailable"`
	CSRF         string                      `json:"csrf"`
	CanGrant     bool                        `json:"can_grant"`
	CanResume    bool                        `json:"can_resume"`
}

func (s *Service) platformTenantSections(ctx context.Context, organization Organization, parallel bool) (platformTenantSections, platformBilling) {
	var sections platformTenantSections
	var billing platformBilling
	var wait sync.WaitGroup
	for _, read := range []func(context.Context){
		func(call context.Context) {
			var result []platformTenantMember
			if s.readPlatformSection(call, organization, "members", &result) {
				sections.Members = &result
			}
		},
		func(call context.Context) {
			var result []platformRunner
			if s.readPlatformSection(call, organization, "runners", &result) {
				sections.Runners = &result
			}
		},
		func(call context.Context) {
			var result []platformProject
			if s.readPlatformSection(call, organization, "projects", &result) {
				sections.Projects = &result
			}
		},
		func(call context.Context) {
			if s.config.Allocation == nil || len(s.config.Allocation.EntitlementAdminToken) < 32 {
				return
			}
			if result, err := s.tenantEntitlements(call, organization); err == nil {
				result.Grants = activeGrants(result.Grants, s.config.now())
				sections.Entitlements = &result
			}
		},
		func(call context.Context) {
			if result, err := s.tenantBillingState(call, organization, false); err == nil {
				billing = platformBilling{Available: true, Status: result.Status, CustomerID: result.CustomerID, Plan: result.Plan, PriceLabel: result.PriceLabel}
			}
		},
	} {
		call := func() {
			call, cancel := context.WithTimeout(ctx, platformCallTimeout)
			defer cancel()
			read(call)
		}
		if parallel {
			wait.Go(call)
		} else {
			call()
		}
	}
	wait.Wait()
	return sections, billing
}

func (s *Service) readPlatformSection(ctx context.Context, organization Organization, section string, result any) bool {
	status, body, err := s.serviceCall(ctx, organization, "/internal/v1/platform/"+section, struct{}{}, nil)
	return err == nil && status == http.StatusOK && json.Unmarshal(body, result) == nil && string(body) != "null"
}

func (p *platformOrganization) applySections(sections platformTenantSections, billing platformBilling) {
	p.Billing = billing
	if sections.Members != nil {
		count := len(*sections.Members)
		p.MemberCount = &count
	}
	if sections.Runners != nil {
		count := len(*sections.Runners)
		p.RunnerCount = &count
	}
	if sections.Entitlements != nil {
		p.Plan = &sections.Entitlements.EffectiveBase.ID
		count := len(sections.Entitlements.Grants)
		p.Grants = &count
	}
}

func (s *Service) platformOrganizationJSON(c echo.Context) error {
	session, allowed, err := s.platformSession(c, "platform_organization_viewed")
	if !allowed {
		return err
	}
	ctx := c.Request().Context()
	id := c.Param("organization")
	organization, err := s.registry.Organization(ctx, id)
	if errors.Is(err, ErrOrganizationNotFound) || err == nil && organization.State == "deleted" {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "That tenant is not registered"})
	}
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "registry_unavailable", "message": "The organization registry is temporarily unavailable"})
	}
	detail := platformTenantDetail{Events: []platformOrganizationEvent{}, Unavailable: []string{}, CSRF: cloudassert.CSRFToken(session.CSRFSecret, ""), CanGrant: s.entitlementAdministrator(ctx, session), CanResume: s.sessionPlatformRole(ctx, session) == "admin" && s.config.Allocation != nil && organization.Managed && provisioningRetryable(organization)}
	item := &detail.Organization
	err = s.registry.store.db.QueryRowContext(ctx, "SELECT id,name,state,step,attempts,error_code,error_detail,managed,creator_email,created_at,updated_at FROM organizations WHERE id = ?", id).Scan(&item.ID, &item.Name, &item.State, &item.Step, &item.Attempts, &item.ErrorCode, &item.ErrorDetail, &item.Managed, &item.CreatorEmail, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "registry_unavailable", "message": "The organization registry is temporarily unavailable"})
	}
	rows, err := s.registry.store.db.QueryContext(ctx, "SELECT event,generation,recorded_at FROM organization_events WHERE organization_id = ? ORDER BY recorded_at,id", id)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "registry_unavailable", "message": "The provisioning timeline is temporarily unavailable"})
	}
	defer rows.Close()
	for rows.Next() {
		var event platformOrganizationEvent
		if err := rows.Scan(&event.Event, &event.Generation, &event.RecordedAt); err != nil {
			return err
		}
		detail.Events = append(detail.Events, event)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	item.CanSupport = item.State == "ready" && s.supportActor(ctx, session.Email)
	if organization.State == "ready" {
		var billing platformBilling
		detail.platformTenantSections, billing = s.platformTenantSections(ctx, organization, true)
		item.applySections(detail.platformTenantSections, billing)
	}
	if s.config.Billing != nil {
		var customer string
		err := s.registry.store.db.QueryRowContext(ctx, "SELECT customer_id FROM billing_customers WHERE organization_id = ? AND mode = ? AND account_id = ?", id, s.config.Billing.Mode, s.config.Billing.AccountID).Scan(&customer)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			item.Billing.CustomerID = customer
		}
	}
	for _, section := range []struct {
		name        string
		unavailable bool
	}{
		{"members", detail.Members == nil}, {"runners", detail.Runners == nil}, {"projects", detail.Projects == nil}, {"entitlements", detail.Entitlements == nil}, {"billing", !item.Billing.Available},
	} {
		if section.unavailable {
			detail.Unavailable = append(detail.Unavailable, section.name)
		}
	}
	return c.JSON(http.StatusOK, detail)
}

func (s *Service) resumePlatformProvisioning(c echo.Context) error {
	session, allowed, err := s.platformSession(c, "platform_provisioning_resume_requested")
	if !allowed {
		return err
	}
	ctx := c.Request().Context()
	if s.sessionPlatformRole(ctx, session) != "admin" {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "forbidden", "message": "Only platform administrators can resume provisioning"})
	}
	if !s.csrfValid(c, session, "") {
		return c.JSON(http.StatusForbidden, map[string]string{"code": "csrf_invalid", "message": "Reload the page and try again"})
	}
	organization, err := s.registry.Organization(ctx, c.Param("organization"))
	if errors.Is(err, ErrOrganizationNotFound) {
		return c.JSON(http.StatusNotFound, map[string]string{"code": "not_found", "message": "That tenant is not registered"})
	}
	if err != nil || s.config.Allocation == nil || !organization.Managed || !provisioningRetryable(organization) {
		return c.JSON(http.StatusConflict, map[string]string{"code": "not_retryable", "message": "That tenant cannot resume provisioning"})
	}
	if _, err := s.resumeOrganization(ctx, organization); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "Setup could not be resumed"})
	}
	return c.JSON(http.StatusOK, map[string]string{"next": "/platform/tenants?tenant=" + organization.ID})
}
