package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

func ValidateHostedRuntimeUpdate(update HostedRuntimeUpdate) error {
	if update.Reason == "" || len(update.Reason) > 500 || update.Plans == nil {
		return nativePublicInvalid("A bounded reload reason and explicit plan configuration are required")
	}
	if !update.Plans.IsZero() {
		if err := update.Plans.validate(); err != nil {
			return err
		}
	}
	if len(update.Prices) > 20 {
		return errors.New("too many hosted billing prices")
	}
	seen := make(map[string]bool, len(update.Prices))
	for _, price := range update.Prices {
		found := slices.ContainsFunc(update.Plans.Plans, func(plan HostedPlan) bool { return plan.PlanReference == price.Plan })
		if !strings.HasPrefix(price.PriceID, "price_") || !hostedSafeID(price.PriceID) || seen[price.PriceID] || strings.TrimSpace(price.Label) == "" || len(price.Label) > 80 || !found || price.Plan == update.Plans.Base {
			return errors.New("hosted billing prices require unique approved paid plans and bounded labels")
		}
		seen[price.PriceID] = true
	}
	return nil
}

type hostedRuntimeConfig struct {
	Revision string
	Plans    HostedPlansConfig
	Billing  *HostedBillingConfig
}

type HostedRuntimeUpdate struct {
	Plans        *HostedPlansConfig   `json:"plans"`
	Prices       []HostedBillingPrice `json:"prices"`
	Reason       string               `json:"reason"`
	ValidateOnly bool                 `json:"validate_only"`
	StageID      string               `json:"stage_id,omitempty"`
}

func (d *database) runtimeState() *hostedRuntimeConfig { return d.hostedRuntime.Load() }
func (d *database) plans() *HostedPlansConfig {
	state := d.runtimeState()
	if state == nil || state.Plans.IsZero() {
		return nil
	}
	return &state.Plans
}

func (s *Service) hostedBillingConfig() *HostedBillingConfig {
	if s.config.Hosted == nil {
		return nil
	}
	if state := s.database.runtimeState(); state != nil && state.Billing != nil {
		return state.Billing
	}
	return s.config.Hosted.Billing
}

func (s *Service) reloadHostedRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	var update HostedRuntimeUpdate
	if err := decodeAPIJSON(c, &update); err != nil {
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
	var update HostedRuntimeUpdate
	if state := s.database.runtimeState(); state != nil {
		update.Plans = &state.Plans
		if state.Billing != nil {
			update.Prices = state.Billing.Prices
		}
	}
	return c.JSON(http.StatusOK, update)
}

func (s *Service) applyHostedRuntime(ctx context.Context, update *HostedRuntimeUpdate) error {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if update.Reason == "" || len(update.Reason) > 500 {
		return nativePublicInvalid("A bounded reload reason is required")
	}
	plans := defaultHostedPlans(s.config.Hosted)
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
		if err := plans.validate(); err != nil {
			return nativePublicInvalid("Reloaded entitlement configuration is invalid")
		}
	}
	var config *HostedBillingConfig
	if current := s.hostedBillingConfig(); current != nil {
		next := *current
		next.Prices = slices.Clone(update.Prices)
		config = &next
		if err := config.validate(&plans); err != nil {
			return nativePublicInvalid("Reloaded billing prices are invalid")
		}
	} else if len(update.Prices) != 0 {
		return nativePublicInvalid("Enabling billing requires a restart")
	}
	actor := s.config.Hosted.EntitlementAdministrator
	if update.StageID != "" {
		if _, err := uuid.Parse(update.StageID); err != nil || !s.hostedShared() {
			return nativePublicInvalid("Runtime preparation requires a shared entry and a publication identity")
		}
	}
	update.Plans = &plans
	if err := s.database.applyRuntimeRecords(ctx, plans, update.Prices, actor, update.Reason, update.StageID, update.ValidateOnly); err != nil {
		return err
	}
	if update.ValidateOnly {
		return nil
	}
	if err := s.database.refreshRuntime(ctx); err != nil {
		return err
	}
	message := "hosted runtime configuration prepared"
	if update.StageID == "" {
		message = "hosted runtime configuration reloaded"
	}
	s.config.Logger.InfoContext(ctx, message, "actor", actor, "reason", update.Reason, "publication", update.StageID)
	return nil
}
