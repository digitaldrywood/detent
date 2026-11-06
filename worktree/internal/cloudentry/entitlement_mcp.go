package cloudentry

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (a platformCredits) entitlements(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	actor, err := a.actor(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	var input operatortool.PlatformEntitlementArguments
	if err := operatortool.DecodeArguments(call.Arguments, &input); err != nil || !safeID(input.OrganizationID) {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	organization, err := a.service.readyOrganization(ctx, input.OrganizationID)
	if err != nil {
		return operatortool.Result{}, err
	}
	var result any
	if call.Name == operatortool.PlatformEntitlements {
		result, err = a.service.tenantEntitlements(ctx, organization)
	} else {
		session := accountSession{Subject: actor, Email: actor}
		result, err = a.service.applyPlatformEntitlement(ctx, session, organization, input.PlatformEntitlementChange)
	}
	if err != nil {
		return operatortool.Result{}, err
	}
	raw, err := json.Marshal(result)
	return operatortool.Result{Content: raw}, err
}
