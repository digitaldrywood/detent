package changefiles

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type ContentDeps struct {
	Repository        tenantstore.Repository
	Clock             func() time.Time
	Plans             func() *hostedplans.HostedPlansConfig
	EntitlementRecord func(context.Context, billingstore.Reader, time.Time) (hostedplans.HostedEntitlement, error)
}

type Content struct {
	deps ContentDeps
}

func NewContent(deps ContentDeps) *Content {
	return &Content{deps: deps}
}
