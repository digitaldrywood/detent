package hubserver

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func (s *Service) hostedPlanPage(c echo.Context) error {
	credential, err := s.hostedAdministrator(c)
	if err != nil {
		return s.hostedError(c, http.StatusForbidden, "Organization plan and usage require owner or admin access")
	}
	now := s.config.now()
	entitlement, err := hubapi.FeatureResult(s.database.plansService().PlanUsage(c.Request().Context(), now))
	if err != nil {
		return s.hostedError(c, http.StatusServiceUnavailable, "Plan information is temporarily unavailable")
	}
	data := templates.HostedPageData{Mode: "plan", Title: "Plan and usage", CanManage: true, CanManageOwnership: credential.HostedRole == "owner"}
	data.PlanName = entitlement.Name
	data.PlanPrice = hostedplans.PlanPrice(entitlement.MonthlyUSDCents)
	data.PlanSource = "Base plan"
	if entitlement.Source == "subscription" {
		data.PlanSource = "Subscription-derived plan"
	}
	data.UsageWindow = entitlement.WindowEndsAt.Format(time.RFC3339)
	for _, name := range hostedplans.AllowanceNames() {
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
	return s.renderHosted(c, http.StatusOK, data)
}
