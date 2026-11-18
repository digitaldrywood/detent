package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type authorityReaderProbe struct {
	tenantstore.Repository
	input              tenantauth.RequestIdentity
	reads, legacyReads int
	err                error
}

func (p *authorityReaderProbe) ReadTenantAuthority(_ context.Context, input tenantauth.RequestIdentity) (tenantauth.Scope, error) {
	p.reads++
	p.input = input
	return tenantauth.Scope{}, p.err
}

func (p *authorityReaderProbe) ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error {
	p.legacyReads++
	return errors.New("unexpected legacy authority read")
}
