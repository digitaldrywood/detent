package changeapi

import (
	"context"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ValidatorDeps struct {
	Repository                        tenantstore.Repository
	RunDetailStoreFor                 func(ctx context.Context) attachment.DetailStore
	RunnerSessionFence                func(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, lock bool) (leasestore.Attempt, policystore.Revision, error)
	ReadRunnerSessionPayload          func(ctx context.Context, record runnersession.Record) (runnersession.Snapshot, error)
	VerifyRunnerSessionFinalization   func(ctx context.Context, scope hubscope.NativeScope, record runnersession.Record, snapshot runnersession.Snapshot) error
	HydrateWorkItemRecord             func(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error)
	PrepareValidatorAuditDispositions func(ctx context.Context, scope hubscope.NativeScope, changeID string, run securityaudit.Run) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error)
	NativeRejection                   func(err error, category, field string) error
	SecurityAuditRejection            func(message, field string) error
	RunnerSessionFenceWithOwner       func(context.Context, tenantstore.Reader, hubscope.NativeScope, runnersession.Mutation, string, runnerstore.Identity) (leasestore.Attempt, corestore.WorkItem, policystore.Revision, error)
}

type validator struct {
	deps ValidatorDeps
}

func newValidator(deps ValidatorDeps) *validator {
	return &validator{deps: deps}
}
