package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type tenantChangeVersionAuthority struct {
	repository tenantstore.Repository
	content    *eventstore.Content
}

func (a tenantChangeVersionAuthority) Prepare(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, now time.Time) (hubrecords.ChangeVersionApproval, error) {
	return a.versionAuthority().Prepare(ctx, scope, change, request, now)
}

func (a tenantChangeVersionAuthority) Authorize(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion, prepared hubrecords.ChangeVersionApproval, now time.Time) error {
	return a.versionAuthority().Authorize(ctx, w, scope, change, request, prepared, now)
}

func (a tenantChangeVersionAuthority) versionAuthority() changerules.VersionAuthority {
	return changerules.VersionAuthority{Repository: a.repository, Content: a.content, DefaultReviewPolicy: defaultChangeReviewPolicy, Reject: nativeRejection}
}

func readCurrentChangePolicyRows(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope) (hubrecords.CurrentChangePolicyRows, error) {
	return changerules.ReadCurrentChangePolicyRows(ctx, r, scope)
}

func hydrateCurrentChangeVersionApproval(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, policyID string) (hubrecords.ChangeVersionApproval, error) {
	return changerules.HydrateCurrentChangeVersionApproval(ctx, content, scope, rows, policyID, defaultChangeReviewPolicy)
}

func hydrateChangeSummaryPolicy(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows) (hubrecords.ChangeVersionApproval, error) {
	return changerules.HydrateChangeSummaryPolicy(ctx, content, scope, rows, defaultChangeReviewPolicy)
}
