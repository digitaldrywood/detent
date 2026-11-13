package hubserver

import (
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (d *database) plansService() *hostedplans.Service {
	return hostedplans.New(hostedplans.Deps{Repository: d.billingRepository, Details: d.billingAuditStore, Organization: d.hostedOrganization, Runtime: &d.hostedRuntime, Loads: &d.runtimeLoads, RuntimeBilling: d.runtimeBilling, HostedBilling: d.hostedBilling, Logger: d.logger, Now: d.now, CurrentTime: d.currentTime, Configured: func(mode string, multiplier float64, credits bool) {
		d.hostedBilling = true
		if credits {
			d.aiCreditMode = mode
			d.aiCreditCostMultiplier = multiplier
		} else {
			d.aiCreditMode = ""
		}
	}})
}

func NewHostedRuntimeSnapshots(p0 tenantstore.Repository, p1 attachment.DetailStore) (*hostedplans.HostedRuntimeSnapshots, error) {
	r0, r1 := hostedplans.NewHostedRuntimeSnapshots(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func ValidateHostedRuntimeUpdate(p0 hostedplans.HostedRuntimeUpdate) error {
	r0 := hostedplans.ValidateHostedRuntimeUpdate(p0)
	return hubapi.FromFeatureError(r0)
}
