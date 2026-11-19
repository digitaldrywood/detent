package changerules

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type QualityEscapesDeps struct {
	PopulationLimit                int
	Repository                     tenantstore.Repository
	AuthorizeAccessScope           func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error
	RunDetailStoreFor              func(ctx context.Context) attachment.DetailStore
	HydrateCanonicalWorkItemRecord func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	HydrateCommentRecord           func(ctx context.Context, row corestore.Comment, version eventstore.CollaborationVersion) (tracker.NativeComment, error)
}

type QualityEscapes struct {
	deps QualityEscapesDeps
}

func NewQualityEscapes(deps QualityEscapesDeps) *QualityEscapes {
	return &QualityEscapes{deps: deps}
}
