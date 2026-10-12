package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

type hostedRuntimeConfig struct {
	Plans   HostedPlansConfig
	Billing *HostedBillingConfig
}

type HostedRuntimeUpdate struct {
	Plans        *HostedPlansConfig   `json:"plans"`
	Prices       []HostedBillingPrice `json:"prices"`
	Reason       string               `json:"reason"`
	ValidateOnly bool                 `json:"validate_only"`
}

func (d *database) plans() *HostedPlansConfig {
	state := d.hostedRuntime.Load()
	if state == nil {
		return nil
	}
	return &state.Plans
}

func (s *Service) hostedBillingConfig() *HostedBillingConfig {
	if s.config.Hosted == nil {
		return nil
	}
	if state := s.database.hostedRuntime.Load(); state != nil && state.Billing != nil {
		return state.Billing
	}
	return s.config.Hosted.Billing
}

func (s *Service) reloadHostedRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return c.NoContent(http.StatusForbidden)
	}
	var update HostedRuntimeUpdate
	if err := decodeAPIJSON(c, &update); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := s.applyHostedRuntime(c.Request().Context(), update); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) readHostedRuntime(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return c.NoContent(http.StatusForbidden)
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	update := HostedRuntimeUpdate{Plans: s.database.plans()}
	if billing := s.hostedBillingConfig(); billing != nil {
		update.Prices = billing.Prices
	}
	return c.JSON(http.StatusOK, update)
}

func (s *Service) applyHostedRuntime(ctx context.Context, update HostedRuntimeUpdate) error {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if update.Reason == "" || len(update.Reason) > 500 {
		return nativeInvalid("A bounded reload reason is required")
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
	if err := plans.validate(); err != nil {
		return nativeInvalid("Reloaded entitlement configuration is invalid")
	}
	var config *HostedBillingConfig
	if current := s.hostedBillingConfig(); current != nil {
		next := *current
		next.Prices = slices.Clone(update.Prices)
		config = &next
		if err := config.validate(&plans); err != nil {
			return nativeInvalid("Reloaded billing prices are invalid")
		}
	} else if len(update.Prices) != 0 {
		return nativeInvalid("Enabling billing requires a restart")
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actor := s.config.Hosted.EntitlementAdministrator
	if err := s.database.seedHostedPlans(ctx, tx, plans, actor, update.Reason); err != nil {
		return err
	}
	if err := seedHostedBillingPrices(ctx, tx, update.Prices); err != nil {
		return err
	}
	if update.ValidateOnly {
		return tx.Rollback()
	}
	command := hostedPlanCommand{ID: "reload_" + uuid.NewString(), Action: "reload", Reason: update.Reason}
	if err := s.database.auditHostedPlanCommand(ctx, tx, actor, command, s.config.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: plans, Billing: config})
	s.config.Logger.InfoContext(ctx, "hosted runtime configuration reloaded", "actor", actor, "reason", update.Reason)
	return nil
}
