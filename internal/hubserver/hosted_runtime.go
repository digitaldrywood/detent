package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) hostedBillingConfig() *hostedplans.HostedBillingConfig {
	if s.config.Hosted == nil {
		return nil
	}
	if state := s.database.plansService().RuntimeState(); state != nil && state.Billing != nil {
		return state.Billing
	}
	return s.config.Hosted.Billing
}

func (s *Service) reloadHostedRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	var update hostedplans.HostedRuntimeUpdate
	if err := hubapi.DecodeAPIJSON(c, &update); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.applyHostedRuntime(c.Request().Context(), &update); err != nil {
		return s.nativeAPIError(c, err)
	}
	if update.StageID != "" && !update.ValidateOnly {
		return c.JSON(http.StatusOK, update)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) readHostedRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	var update hostedplans.HostedRuntimeUpdate
	if state := s.database.plansService().RuntimeState(); state != nil {
		update.Plans = &state.Plans
		if state.Billing != nil {
			update.Prices = state.Billing.Prices
		}
	}
	return c.JSON(http.StatusOK, update)
}

func (s *Service) applyHostedRuntime(ctx context.Context, update *hostedplans.HostedRuntimeUpdate) error {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if update.Reason == "" || len(update.Reason) > 500 {
		return hubapi.NativePublicInvalid("A bounded reload reason is required")
	}
	plans := hostedplans.DefaultPlans(hostedplans.PlansInput(s.config.Hosted))
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
	var config *hostedplans.HostedBillingConfig
	if current := s.hostedBillingConfig(); current != nil {
		next := *current
		next.Prices = slices.Clone(update.Prices)
		config = &next
		if err := config.Validate(&plans); err != nil {
			return hubapi.NativePublicInvalid("Reloaded billing prices are invalid")
		}
	} else if len(update.Prices) != 0 {
		return hubapi.NativePublicInvalid("Enabling billing requires a restart")
	}
	actor := s.config.Hosted.EntitlementAdministrator
	if update.StageID != "" {
		if _, err := uuid.Parse(update.StageID); err != nil || !s.hostedShared() {
			return hubapi.NativePublicInvalid("Runtime preparation requires a shared entry and a publication identity")
		}
	}
	update.Plans = &plans
	if err := hubapi.FromFeatureError(s.database.plansService().ApplyRuntimeRecords(ctx, plans, update.Prices, actor, update.Reason, update.StageID, update.ValidateOnly)); err != nil {
		return err
	}
	if update.ValidateOnly {
		return nil
	}
	if err := hubapi.FromFeatureError(s.database.plansService().RefreshRuntime(ctx)); err != nil {
		return err
	}
	message := "hosted runtime configuration prepared"
	if update.StageID == "" {
		message = "hosted runtime configuration reloaded"
	}
	s.config.Logger.InfoContext(ctx, message, "actor", actor, "reason", update.Reason, "publication", update.StageID)
	return nil
}
