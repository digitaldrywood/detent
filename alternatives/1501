package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

type hostedRuntimeConfig struct {
	Plans     HostedPlansConfig
	Billing   *HostedBillingConfig
	Published []string
	File      os.FileInfo
}

const HostedRuntimeFilename = "cloud-runtime.json"

type HostedRuntimePublication struct {
	Published []string                       `json:"published"`
	Update    HostedRuntimeUpdate            `json:"update"`
	Tenants   map[string]HostedRuntimeUpdate `json:"tenants"`
}

type HostedRuntimeUpdate struct {
	Plans        *HostedPlansConfig   `json:"plans"`
	Prices       []HostedBillingPrice `json:"prices"`
	Reason       string               `json:"reason"`
	ValidateOnly bool                 `json:"validate_only"`
	StageID      string               `json:"stage_id,omitempty"`
}

func (d *database) runtimeState() *hostedRuntimeConfig {
	d.runtimeReadMu.Lock()
	defer d.runtimeReadMu.Unlock()
	current := d.hostedRuntime.Load()
	if d.runtimeFile == "" {
		return current
	}
	file, err := os.Stat(d.runtimeFile)
	if err != nil || current != nil && current.File != nil && os.SameFile(file, current.File) && file.ModTime().Equal(current.File.ModTime()) && file.Size() == current.File.Size() {
		return current
	}
	raw, err := os.ReadFile(d.runtimeFile)
	if err != nil {
		return current
	}
	var publication HostedRuntimePublication
	if json.Unmarshal(raw, &publication) != nil {
		return current
	}
	update, ok := publication.Tenants[string(d.hostedOrganization)]
	if !ok || !slices.Contains(publication.Published, update.StageID) {
		return current
	}
	next := hostedRuntimeConfig{}
	if current != nil {
		next = *current
	}
	next.File, next.Published = file, publication.Published
	next.Plans = HostedPlansConfig{}
	if !update.Plans.IsZero() {
		next.Plans = *update.Plans
	}
	if d.runtimeBilling != nil {
		billing := *d.runtimeBilling
		billing.Prices = update.Prices
		next.Billing = &billing
	}
	d.hostedRuntime.Store(&next)
	return &next
}

func (d *database) runtimeVisible(id string) bool {
	state := d.runtimeState()
	return id == "" || state != nil && slices.Contains(state.Published, id)
}

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
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	update := HostedRuntimeUpdate{Plans: s.database.plans()}
	if billing := s.hostedBillingConfig(); billing != nil {
		update.Prices = billing.Prices
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
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actor := s.config.Hosted.EntitlementAdministrator
	if update.StageID != "" {
		if _, err := uuid.Parse(update.StageID); err != nil || !s.hostedShared() {
			return nativePublicInvalid("Runtime preparation requires a shared entry and a publication identity")
		}
	}
	if err := s.database.seedHostedPlans(ctx, tx, plans, actor, update.Reason, update.StageID); err != nil {
		return err
	}
	if err := seedHostedBillingPrices(ctx, tx, update.Prices); err != nil {
		return err
	}
	update.Plans = &plans
	if update.ValidateOnly {
		return tx.Rollback()
	}
	command := hostedPlanCommand{ID: "reload_" + uuid.NewString(), Action: "reload", Reason: update.Reason}
	if update.StageID != "" {
		command.Action = "prepare_reload"
	}
	if err := s.database.auditHostedPlanCommand(ctx, tx, actor, command, s.config.now()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if update.StageID == "" {
		state := &hostedRuntimeConfig{Plans: plans, Billing: config}
		if current := s.database.runtimeState(); current != nil {
			state.File, state.Published = current.File, current.Published
		}
		s.database.hostedRuntime.Store(state)
	}
	message := "hosted runtime configuration prepared"
	if update.StageID == "" {
		message = "hosted runtime configuration reloaded"
	}
	s.config.Logger.InfoContext(ctx, message, "actor", actor, "reason", update.Reason, "publication", update.StageID)
	return nil
}

func (d *database) configureRuntimePublication(cfg Config) {
	if cfg.Hosted != nil && cfg.Hosted.SharedEntry != nil {
		d.runtimeFile = filepath.Join(filepath.Dir(d.path), HostedRuntimeFilename)
		d.runtimeBilling = cfg.Hosted.Billing
	}
}
