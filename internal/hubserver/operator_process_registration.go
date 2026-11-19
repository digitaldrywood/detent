package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
)

func registerSessionWorkerProcess(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, record runnersession.Record, snapshot runnersession.Snapshot, history attachment.ArchiveReference, fence leasestore.Fence) error {
	return operatorcommands.RegisterSessionWorkerProcess(ctx, w, scope, record, snapshot, history, fence)
}
