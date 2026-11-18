package leasestore

import (
	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttemptIdentity struct {
	Execution *tracker.NativeExecutionIdentity
	Runtime   agentidentity.Identity
}

func NewAttemptIdentity(data tracker.NativeRunData) AttemptIdentity {
	var identity AttemptIdentity
	if data.Identity != nil && data.Identity.Role != "" && data.Identity.Backend != "" && data.Identity.Model != "" {
		execution := *data.Identity
		identity.Execution = &execution
	}
	if data.Runtime != nil {
		identity.Runtime = data.Runtime.Identity.Normalize()
	}
	return identity
}

func (i AttemptIdentity) Merge(update AttemptIdentity) AttemptIdentity {
	if update.Execution != nil {
		execution := *update.Execution
		i.Execution = &execution
	}
	i.Runtime = i.Runtime.Merge(update.Runtime)
	return i
}
