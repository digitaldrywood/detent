package nativechange

import (
	"github.com/digitaldrywood/detent/internal/hubclient/provider"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) validateProviderStart(identity tracker.NativeExecutionIdentity) error {
	return provider.ValidateStart(e.claim.lease.ProviderReservation, identity, e.scheduler.providerReports, e.unavailable)
}

func (e *nativeExecution) ProviderCapacity() *providercapacity.Reservation {
	return e.claim.lease.ProviderReservation
}
