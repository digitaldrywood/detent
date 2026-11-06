package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseDeliveryRows struct {
	current, source  corestore.WorkItem
	project          projectstore.Record
	version          eventstore.CollaborationVersion
	deliveryRow      eventstore.ReleaseDelivery
	found            bool
	checkoutVerified bool
	change, prior    eventstore.Change
	sourceVersion    eventstore.Version
	policy           currentChangePolicyRows
	facts            eventstore.ReleaseDeliveryFacts
	lease            leasestore.Lease
	attempt          tenantAttemptSnapshot
	reviews          []eventstore.Evidence
}

type releaseDeliveryCheck struct {
	releaseDeliveryRows
	issue      tracker.NativeIssue
	existing   tracker.ReleaseDelivery
	approval   ChangeVersionApproval
	request    tracker.AssociateReleasePick
	publishing bool
}
