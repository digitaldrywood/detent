package hostedplans

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

type HTTPDeps struct {
	Plans          func() *Service
	Hosted         func() *HostedConfig
	Logger         func() *slog.Logger
	Now            func() time.Time
	RuntimeMu      *sync.Mutex
	Error          func(echo.Context, error) error
	InvalidRequest func(echo.Context, error) error
	JSONError      func(echo.Context, int, string) error
	Administrator  func(echo.Context) (string, error)
	Render         func(echo.Context, int, templates.HostedPageData) error
	PageError      func(echo.Context, int, string) error
}

type HTTPService struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTPService { return &HTTPService{deps: deps} }

func RegisterRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.GET("/organization/plan", s.planPage)
	e.GET("/api/v2/organizations/:organization/entitlements", s.PlanReport, s.RefreshRuntime)
	e.POST("/api/v2/organizations/:organization/entitlements", s.UpdatePlan, s.RefreshRuntime)
	e.POST("/api/v2/organizations/:organization/entitlements/runtime", s.ReloadRuntime, s.RefreshRuntime)
	e.GET("/api/v2/organizations/:organization/entitlements/runtime", s.ReadRuntime, s.RefreshRuntime)
}

func (d *Service) BillingConfig(hosted *HostedConfig) *HostedBillingConfig {
	if state := d.RuntimeState(); state != nil && state.Billing != nil {
		return state.Billing
	}
	return hosted.Billing
}

func (s *HTTPService) billingConfig() *HostedBillingConfig {
	hosted := s.deps.Hosted()
	if hosted == nil {
		return nil
	}
	return s.deps.Plans().BillingConfig(hosted)
}

func (s *HTTPService) entitlementAdministrator(c echo.Context) bool {
	token, err := hubauth.BearerToken(c)
	supplied := sha256.Sum256([]byte(token))
	configured := sha256.Sum256(s.deps.Hosted().EntitlementAdminToken)
	return err == nil && len(s.deps.Hosted().EntitlementAdminToken) >= 32 && subtle.ConstantTimeCompare(supplied[:], configured[:]) == 1
}

func (s *HTTPService) PlanReport(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.deps.Hosted().OrganizationID {
		return s.deps.JSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	after := int64(0)
	if value := c.QueryParam("history_after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return s.deps.InvalidRequest(c, errors.New("invalid grant history cursor"))
		}
		after = parsed
	}
	limit := 100
	if value := c.QueryParam("history_limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return s.deps.InvalidRequest(c, errors.New("grant history limit must be between 1 and 100"))
		}
		limit = parsed
	}
	report, err := hubapi.FeatureResult(s.deps.Plans().Entitlements(c.Request().Context(), after, limit))
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *HTTPService) UpdatePlan(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.deps.Hosted().OrganizationID {
		return s.deps.JSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}

	var command PlanCommand
	if err := hubapi.DecodeAPIJSON(c, &command); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if err := hubapi.FromFeatureError(s.deps.Plans().ApplyPlanCommand(c.Request().Context(), s.deps.Hosted().EntitlementAdministrator, command)); err != nil {
		return s.deps.Error(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPService) ReloadRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.deps.Hosted().OrganizationID {
		return s.deps.JSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	var update HostedRuntimeUpdate
	if err := hubapi.DecodeAPIJSON(c, &update); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if err := s.applyRuntime(c.Request().Context(), &update); err != nil {
		return s.deps.Error(c, err)
	}
	if update.StageID != "" && !update.ValidateOnly {
		return c.JSON(http.StatusOK, update)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPService) ReadRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.deps.Hosted().OrganizationID {
		return s.deps.JSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	var update HostedRuntimeUpdate
	if state := s.deps.Plans().RuntimeState(); state != nil {
		update.Plans = &state.Plans
		if state.Billing != nil {
			update.Prices = state.Billing.Prices
		}
	}
	return c.JSON(http.StatusOK, update)
}

func (s *HTTPService) applyRuntime(ctx context.Context, update *HostedRuntimeUpdate) error {
	s.deps.RuntimeMu.Lock()
	defer s.deps.RuntimeMu.Unlock()
	if update.Reason == "" || len(update.Reason) > 500 {
		return hubapi.NativePublicInvalid("A bounded reload reason is required")
	}
	hosted := s.deps.Hosted()
	plans := DefaultPlans(PlansInput(hosted))
	if !update.Plans.IsZero() {
		raw, err := json.Marshal(update.Plans)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &plans); err != nil {
			return err
		}
	}
	if !plans.IsZero() {
		if err := plans.Validate(); err != nil {
			return hubapi.NativePublicInvalid("Reloaded entitlement configuration is invalid")
		}
	}
	var config *HostedBillingConfig
	if current := s.billingConfig(); current != nil {
		next := *current
		next.Prices = slices.Clone(update.Prices)
		config = &next
		if err := config.Validate(&plans); err != nil {
			return hubapi.NativePublicInvalid("Reloaded billing prices are invalid")
		}
	} else if len(update.Prices) != 0 {
		return hubapi.NativePublicInvalid("Enabling billing requires a restart")
	}
	actor := hosted.EntitlementAdministrator
	if update.StageID != "" {
		if _, err := uuid.Parse(update.StageID); err != nil || hosted.SharedEntry == nil {
			return hubapi.NativePublicInvalid("Runtime preparation requires a shared entry and a publication identity")
		}
	}
	update.Plans = &plans
	if err := hubapi.FromFeatureError(s.deps.Plans().ApplyRuntimeRecords(ctx, plans, update.Prices, actor, update.Reason, update.StageID, update.ValidateOnly)); err != nil {
		return err
	}
	if update.ValidateOnly {
		return nil
	}
	if err := hubapi.FromFeatureError(s.deps.Plans().RefreshRuntime(ctx)); err != nil {
		return err
	}
	message := "hosted runtime configuration prepared"
	if update.StageID == "" {
		message = "hosted runtime configuration reloaded"
	}
	s.deps.Logger().InfoContext(ctx, message, "actor", actor, "reason", update.Reason, "publication", update.StageID)
	return nil
}

func (s *HTTPService) RefreshRuntime(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if !s.entitlementAdministrator(c) || c.Param("organization") != s.deps.Hosted().OrganizationID {
			return next(c)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
		defer cancel()
		if err := hubapi.FromFeatureError(s.deps.Plans().RefreshRuntime(ctx)); err != nil {
			s.deps.Logger().WarnContext(ctx, "hosted runtime refresh failed; retaining last snapshot", "error", err)
		}
		return next(c)
	}
}

func (s *HTTPService) planPage(c echo.Context) error {
	role, err := s.deps.Administrator(c)
	if err != nil {
		return s.deps.PageError(c, http.StatusForbidden, "Organization plan and usage require owner or admin access")
	}
	now := s.deps.Now()
	entitlement, err := hubapi.FeatureResult(s.deps.Plans().PlanUsage(c.Request().Context(), now))
	if err != nil {
		return s.deps.PageError(c, http.StatusServiceUnavailable, "Plan information is temporarily unavailable")
	}
	data := templates.HostedPageData{Mode: "plan", Title: "Plan and usage", CanManage: true, CanManageOwnership: role == "owner"}
	data.PlanName = entitlement.Name
	data.PlanPrice = PlanPrice(entitlement.MonthlyUSDCents)
	data.PlanSource = "Base plan"
	if entitlement.Source == "subscription" {
		data.PlanSource = "Subscription-derived plan"
	}
	data.UsageWindow = entitlement.WindowEndsAt.Format(time.RFC3339)
	for _, name := range AllowanceNames() {
		allowance, limited := entitlement.Allowances[name]
		if !limited {
			continue
		}
		used := entitlement.Usage[name]
		data.Allowances = append(data.Allowances, templates.HostedAllowanceRow{Label: strings.ReplaceAll(strings.ReplaceAll(name, "api_", "API_"), "_", " "), Consumption: strconv.FormatInt(used, 10), Allowance: strconv.FormatInt(allowance, 10), Remaining: strconv.FormatInt(max(allowance-used, 0), 10), OverLimit: used > allowance, LimitOnly: slices.Contains([]string{"artifact_bytes", "artifact_upload_bytes", "artifact_retention_seconds"}, name)})
	}
	for _, grant := range entitlement.Grants {
		status := "Active complimentary access"
		switch {
		case grant.RevokedAt != nil:
			status = "Revoked complimentary access"
		case grant.ExpiresAt != nil && !grant.ExpiresAt.After(now):
			status = "Expired complimentary access"
		case grant.StartsAt.After(now):
			status = "Scheduled complimentary access"
		}
		if grant.ExpiresAt != nil {
			status += " · expiry " + grant.ExpiresAt.UTC().Format(time.RFC3339)
		} else {
			status += " · no expiry"
		}
		data.PlanGrants = append(data.PlanGrants, status+" · "+strings.Join(grant.Scope, ", "))
	}
	return s.deps.Render(c, http.StatusOK, data)
}
