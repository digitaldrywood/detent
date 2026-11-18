package workitemread

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Tenants      tenantstore.Repository
	Access       tenantstore.Repository
	Details      attachment.DetailStore
	Now          func() time.Time
	Hosted       bool
	PublicURL    string
	Organization tracker.OrganizationID
	Authority    func(hubscope.NativeScope, bool) accessstore.Authority
	Authorize    func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
}

type Reader struct {
	deps Deps
}

func New(deps Deps) Reader {
	return Reader{deps: deps}
}

func (rd Reader) accessRead(ctx context.Context, organization tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	if rd.deps.Access == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	return rd.deps.Access.ReadTenant(ctx, organization, corestore.Serving, read)
}
