package changeapi

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RecordReaderDeps struct {
	Repository                              tenantstore.Repository
	RunDetailStore                          func() attachment.DetailStore
	ReadWorkItemRecord                      func(ctx context.Context, scope hubscope.NativeScope, id string) (corestore.WorkItem, eventstore.CollaborationVersion, error)
	HydrateWorkItemRecord                   func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	HydrateChangeVersion                    func(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version) (tracker.ChangeVersion, error)
	HydrateChangeVersionWithRows            func(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version, captured *tenantstore.ChangeVersionRows) (tracker.ChangeVersion, error)
	RunDetailStoreFor                       func(ctx context.Context) attachment.DetailStore
	SummarizeChangeWithAuditDispositions    func(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool) (tracker.ChangeSummary, error)
	SummarizeChangeWithAuditDispositionsFor func(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool, workload corestore.Workload, facts *tenantstore.BoardChangeRows) (tracker.ChangeSummary, error)
	ReadBoundedChangePushes                 func(ctx context.Context, scope hubscope.NativeScope, item string, versions []tracker.ChangeVersion) ([]tracker.ChangePublicationPush, error)
	HydrateChangeSummaryPolicy              func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows) (hubrecords.ChangeVersionApproval, error)
	BoardCurrentApproval                    func(ctx context.Context, content *eventstore.Content, scope hubscope.NativeScope, rows hubrecords.CurrentChangePolicyRows, approval *hubrecords.ChangeVersionApproval) (hubrecords.ChangeVersionApproval, error)
	FilterBoardChangeRecord                 func(scope hubscope.NativeScope, change tracker.ChangeRequest, rows tenantstore.BoardChangeRows) (tracker.ChangeRequest, bool, error)
	ChangeVersionResponse                   func(version tracker.ChangeVersion) tracker.ChangeVersion
	ExternalPullRequestNumber               func(reference tracker.ChangeExternalReference) int
	SplitRepositoryFullName                 func(value string) (string, string, bool)
}

type RecordReader struct {
	deps RecordReaderDeps
}

func NewRecordReader(deps RecordReaderDeps) *RecordReader {
	return &RecordReader{deps: deps}
}
