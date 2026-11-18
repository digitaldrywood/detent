package hostedplans

import (
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func ValidateHostedRuntimeUpdate(update HostedRuntimeUpdate) error {
	if update.Reason == "" || len(update.Reason) > 500 || update.Plans == nil {
		return hubapi.PublicInvalid("A bounded reload reason and explicit plan configuration are required")
	}
	if !update.Plans.IsZero() {
		if err := update.Plans.Validate(); err != nil {
			return err
		}
	}
	if len(update.Prices) > 20 {
		return errors.New("too many hosted billing prices")
	}
	seen := make(map[string]bool, len(update.Prices))
	for _, price := range update.Prices {
		found := slices.ContainsFunc(update.Plans.Plans, func(plan HostedPlan) bool { return plan.PlanReference == price.Plan })
		if !strings.HasPrefix(price.PriceID, "price_") || !SafeID(price.PriceID) || seen[price.PriceID] || strings.TrimSpace(price.Label) == "" || len(price.Label) > 80 || !found || price.Plan == update.Plans.Base {
			return errors.New("hosted billing prices require unique approved paid plans and bounded labels")
		}
		seen[price.PriceID] = true
	}
	return nil
}

type RuntimeConfig struct {
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

func (d *Service) RuntimeState() *RuntimeConfig { return d.hostedRuntime.Load() }

func (d *Service) Plans() *HostedPlansConfig {
	state := d.RuntimeState()
	if state == nil || state.Plans.IsZero() {
		return nil
	}
	return &state.Plans
}
