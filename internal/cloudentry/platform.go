package cloudentry

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
)

const (
	platformPath            = "/platform"
	platformCallTimeout     = 3 * time.Second
	platformRequestDeadline = 8 * time.Second
	platformTenantWorkers   = 8
)

func (s *Service) platformIdentity(ctx context.Context, email string, identity auth.HostedIdentity) bool {
	return s.platformRole(ctx, email) != "" && identity.SupportActor == ""
}

func (s *Service) platformStaff(ctx context.Context, session accountSession) bool {
	return s.platformIdentity(ctx, session.Email, session.Identity)
}

func (s *Service) platformSession(c echo.Context, event string) (accountSession, bool, error) {
	session, err := s.session(c)
	if err != nil {
		return accountSession{}, false, c.JSON(http.StatusUnauthorized, map[string]string{"code": "unauthenticated", "message": "Sign in to continue"})
	}
	if !s.platformStaff(c.Request().Context(), session) {
		return accountSession{}, false, c.JSON(http.StatusForbidden, map[string]string{"code": "forbidden", "message": "The platform console is limited to Detent staff"})
	}
	if err := s.auth.audit(c.Request().Context(), session.Subject, "", event); err != nil {
		return accountSession{}, false, c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "unavailable", "message": "The platform console is temporarily unavailable"})
	}
	return session, true, nil
}

func (s *Service) platformPage(c echo.Context) error {
	session, err := s.session(c)
	if err != nil {
		return c.Redirect(http.StatusSeeOther, "/auth/oidc/start?return=%2Fplatform")
	}
	if !s.platformStaff(c.Request().Context(), session) {
		return s.denied(c, http.StatusForbidden, "The platform console is limited to Detent staff")
	}
	if err := s.auth.audit(c.Request().Context(), session.Subject, "", "platform_opened"); err != nil {
		return s.denied(c, http.StatusServiceUnavailable, "The platform console is temporarily unavailable")
	}
	if served, err := s.clientShell(c); served || err != nil {
		return err
	}
	return s.denied(c, http.StatusServiceUnavailable, "The platform console needs the Detent Cloud client, which this build does not include")
}

type platformBilling struct {
	Available bool   `json:"available"`
	Status    string `json:"status,omitempty"`
}

type platformOrganization struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	State        string          `json:"state"`
	Step         string          `json:"step"`
	Attempts     int             `json:"attempts"`
	ErrorCode    string          `json:"error_code"`
	ErrorDetail  string          `json:"error_detail"`
	Managed      bool            `json:"managed"`
	CreatorEmail string          `json:"creator_email"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	Billing      platformBilling `json:"billing"`
	CanSupport   bool            `json:"can_support"`
	Plan         *string         `json:"plan"`
	Grants       *int            `json:"grants"`
	MemberCount  *int            `json:"member_count"`
	RunnerCount  *int            `json:"runner_count"`
}

var platformUnavailable = []string{"plan", "grants", "member_count", "runner_count"}

func (s *Service) platformOrganizations(ctx context.Context) ([]platformOrganization, error) {
	rows, err := s.registry.store.db.QueryContext(ctx, "SELECT id,name,state,step,attempts,error_code,error_detail,managed,creator_email,created_at,updated_at FROM organizations WHERE state != 'deleted' ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []platformOrganization{}
	for rows.Next() {
		var item platformOrganization
		if err := rows.Scan(&item.ID, &item.Name, &item.State, &item.Step, &item.Attempts, &item.ErrorCode, &item.ErrorDetail, &item.Managed, &item.CreatorEmail, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) eachReadyTenant(parent context.Context, ids []string, visit func(context.Context, int, Organization)) {
	type target struct {
		index        int
		organization Organization
	}
	var targets []target
	for index, id := range ids {
		if organization, err := s.readyOrganization(parent, id); err == nil {
			targets = append(targets, target{index, organization})
		}
	}
	deadline := s.config.platformDeadline
	if deadline == 0 {
		deadline = platformRequestDeadline
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	work := make(chan target)
	var wait sync.WaitGroup
	for range min(platformTenantWorkers, len(targets)) {
		wait.Go(func() {
			for item := range work {
				call, stop := context.WithTimeout(ctx, platformCallTimeout)
				visit(call, item.index, item.organization)
				stop()
			}
		})
	}
feed:
	for _, item := range targets {
		select {
		case work <- item:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wait.Wait()
}

func (s *Service) platformOrganizationsJSON(c echo.Context) error {
	session, allowed, err := s.platformSession(c, "platform_organizations_viewed")
	if !allowed {
		return err
	}
	ctx := c.Request().Context()
	organizations, err := s.platformOrganizations(ctx)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"code": "registry_unavailable", "message": "The organization registry is temporarily unavailable"})
	}
	canSupport := s.supportActor(c.Request().Context(), session.Email)
	ids := make([]string, len(organizations))
	for index, organization := range organizations {
		ids[index] = organization.ID
		organizations[index].CanSupport = canSupport && organization.State == "ready"
	}
	s.eachReadyTenant(ctx, ids, func(call context.Context, index int, organization Organization) {
		if state, err := s.tenantBillingState(call, organization, false); err == nil {
			organizations[index].Billing = platformBilling{Available: true, Status: state.Status}
		}
	})
	return c.JSON(http.StatusOK, map[string]any{
		"email": session.Email, "csrf": cloudassert.CSRFToken(session.CSRFSecret, ""), "can_support": canSupport, "can_grant": s.entitlementAdministrator(ctx, session),
		"organizations": organizations, "unavailable": platformUnavailable,
	})
}

func (s *Service) platformAllowlistJSON(c echo.Context) error {
	if _, allowed, err := s.platformSession(c, "platform_allowlist_viewed"); !allowed {
		return err
	}
	result := map[string]any{
		"self_service": s.config.Allocation != nil, "allowed_emails": []string{}, "allowed_domains": []string{},
		"source": map[string]any{"file": s.config.ConfigPath, "keys": []string{"allocation.allowed_emails", "allocation.allowed_domains"}},
	}
	if allocation := s.config.Allocation; allocation != nil {
		if allocation.AllowedEmails != nil {
			result["allowed_emails"] = allocation.AllowedEmails
		}
		if allocation.AllowedDomains != nil {
			result["allowed_domains"] = allocation.AllowedDomains
		}
		result["open"] = len(allocation.AllowedEmails) == 0 && len(allocation.AllowedDomains) == 0
	}
	return c.JSON(http.StatusOK, result)
}

type platformAdmission struct {
	Tenants                 int    `json:"tenants"`
	MaxTenants              int    `json:"max_tenants"`
	Allocating              int    `json:"allocating"`
	MaxConcurrent           int    `json:"max_concurrent"`
	DiskMeasured            bool   `json:"disk_measured"`
	FreeDiskBytes           uint64 `json:"free_disk_bytes"`
	MinFreeDiskBytes        uint64 `json:"min_free_disk_bytes"`
	MemoryMeasured          bool   `json:"memory_measured"`
	AvailableMemoryBytes    uint64 `json:"available_memory_bytes"`
	MinAvailableMemoryBytes uint64 `json:"min_available_memory_bytes"`
}

func (s *Service) platformHealthJSON(c echo.Context) error {
	if _, allowed, err := s.platformSession(c, "platform_health_viewed"); !allowed {
		return err
	}
	ctx := c.Request().Context()
	organizations, err := s.registry.List(ctx)
	registry := map[string]any{"ok": err == nil}
	result := map[string]any{"registry": registry}
	if err != nil {
		return c.JSON(http.StatusOK, result)
	}
	expected, running := s.servingTenants(ctx, organizations)
	result["tenants"] = map[string]int{"expected": expected, "running": running}
	if allocation := s.config.Allocation; allocation != nil {
		holding, allocating, err := s.admissionLoad(ctx, "")
		if err != nil {
			registry["ok"] = false
			return c.JSON(http.StatusOK, result)
		}
		admission := platformAdmission{Tenants: holding, MaxTenants: allocation.MaxTenants, Allocating: allocating, MaxConcurrent: allocation.MaxConcurrent,
			MinFreeDiskBytes: allocation.MinFreeDiskBytes, MinAvailableMemoryBytes: allocation.MinAvailableMemoryBytes}
		admission.FreeDiskBytes, admission.DiskMeasured = freeDiskBytes(allocation.TenantRoot)
		admission.AvailableMemoryBytes, admission.MemoryMeasured = availableMemoryBytes()
		result["admission"] = admission
	}
	return c.JSON(http.StatusOK, result)
}
