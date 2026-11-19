package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
)

type providerCapacitySnapshot struct {
	truncated    bool
	reservations []runnerfleet.ProviderReservation
}

func runtimeProviderCapacity(snapshot runnerstore.RuntimeProviderSnapshot) providerCapacitySnapshot {
	result := runnerfleet.RuntimeProviderCapacity(landingCapacityKey, snapshot)
	return providerCapacitySnapshot{truncated: result.Truncated, reservations: result.Reservations}
}
