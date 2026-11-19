package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changeapi"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *Service) getAuditDispositions(c echo.Context) error {
	return s.changeAuditEvidence().GetAuditDispositions(c)
}

func (s *Service) getAuditEvidence(c echo.Context) error {
	return s.changeAuditEvidence().GetAuditEvidence(c)
}

func (s *Service) prepareValidatorAuditDispositions(ctx context.Context, scope hubscope.NativeScope, changeID string, run securityaudit.Run) ([]securityaudit.Disposition, []eventstore.AuditDisposition, error) {
	return s.changeAuditEvidence().PrepareValidatorAuditDispositions(ctx, scope, changeID, run)
}

func lockValidatorAuditDispositions(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, runID int64, expected []eventstore.AuditDisposition) error {
	return releaseaudits.LockValidatorAuditDispositions(ctx, r, scope, runID, expected)
}

func (s *Service) summarizeChangeWithAuditDispositions(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool) (tracker.ChangeSummary, error) {
	return s.changeAuditEvidence().SummarizeChangeWithAuditDispositions(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval)
}

func (s *Service) summarizeChangeWithAuditDispositionsFor(ctx context.Context, scope hubscope.NativeScope, detail tracker.ChangeDetail, policyID, reviewPolicyID string, now time.Time, priorApproval bool, workload corestore.Workload, facts *tenantstore.BoardChangeRows) (tracker.ChangeSummary, error) {
	return s.changeAuditEvidence().SummarizeChangeWithAuditDispositionsFor(ctx, scope, detail, policyID, reviewPolicyID, now, priorApproval, workload, facts)
}

func (s *Service) postAuditDisposition(c echo.Context) error {
	return s.changeAuditEvidence().PostAuditDisposition(c)
}

func (s *Service) changeAuditEvidence() *releaseaudits.ChangeEvidence {
	return releaseaudits.NewChangeEvidence(releaseaudits.ChangeEvidenceDeps{
		Repository:                    s.config.TenantRepository,
		Clock:                         s.config.Clock,
		AuthorizeAccessScope:          s.authorizeAccessScope,
		RunDetailStore:                s.runDetailStore,
		NativeAPIError:                func(c echo.Context, err error) error { return s.nativeAPIError(c, err) },
		InvalidAPIRequest:             s.invalidAPIRequest,
		RunnerSessionFence:            s.runnerSessionFence,
		ValidateNativeDetailReference: validateNativeDetailReference,
	})
}

func (s *Service) hydrateChangeVersion(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version) (tracker.ChangeVersion, error) {
	return s.changeObservations().HydrateChangeVersion(ctx, scope, item, row)
}

func (s *Service) hydrateChangeVersionWithRows(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, row eventstore.Version, captured *tenantstore.ChangeVersionRows) (tracker.ChangeVersion, error) {
	return s.changeObservations().HydrateChangeVersionWithRows(ctx, scope, item, row, captured)
}

func sameNativeLandingObservation(left, right *tracker.NativeLandingReceipt) bool {
	return changelanding.SameNativeLandingObservation(left, right)
}

func (s *Service) readNativeLandingObservation(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, ref tracker.NativeChangeReference) (*tracker.NativeLandingReceipt, string, error) {
	return s.changeObservations().ReadNativeLandingObservation(ctx, scope, item, ref)
}

func (s *Service) changeObservations() *changelanding.Observations {
	return changelanding.NewObservations(changelanding.ObservationsDeps{
		Repository:        s.config.TenantRepository,
		RunDetailStoreFor: s.runDetailStoreFor,
	})
}

type boundedChangeRecordCursor = changeapi.BoundedChangeRecordCursor

func encodeBoundedChangeRecordCursor(cursor boundedChangeRecordCursor) (string, error) {
	return changeapi.EncodeBoundedChangeRecordCursor(cursor)
}

func (s *Service) readBoundedChangeRecord(ctx context.Context, scope hubscope.NativeScope, args operatortool.ChangeArguments, result operatortool.ChangeResult) (operatortool.ChangeResult, error) {
	return s.changeReads().ReadBoundedChangeRecord(ctx, scope, args, result)
}

func (s *Service) readExactChangeVersionRecord(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID string) (tracker.ChangeVersion, error) {
	return s.changeReads().ReadExactChangeVersionRecord(ctx, scope, item, changeID, versionID)
}

func (s *Service) recheckChangeReadAuthority(ctx context.Context, scope hubscope.NativeScope) error {
	return s.changeReads().RecheckChangeReadAuthority(ctx, scope)
}

func (s *Service) readBoundedChangePushes(ctx context.Context, scope hubscope.NativeScope, item string, versions []tracker.ChangeVersion) ([]tracker.ChangePublicationPush, error) {
	return s.changeReads().ReadBoundedChangePushes(ctx, scope, item, versions)
}

func (s *Service) changeReads() *changeapi.Reads {
	return changeapi.NewReads(changeapi.ReadsDeps{
		Repository:                           s.config.TenantRepository,
		Clock:                                s.config.Clock,
		RunDetailStoreFor:                    s.runDetailStoreFor,
		AuthorizeAccessScope:                 s.authorizeAccessScope,
		ReadChangeRecord:                     s.readChangeRecord,
		ReadChangeVersionRecord:              s.readChangeVersionRecord,
		HydrateChangeVersion:                 s.hydrateChangeVersion,
		ReadWorkItemRecord:                   s.readWorkItemRecord,
		HydrateWorkItemRecord:                s.hydrateWorkItemRecord,
		LoadChangeExternalRecord:             s.loadChangeExternalRecord,
		AccessAuthority:                      s.accessAuthority,
		ReadNativeAttemptSnapshotTenant:      s.readNativeAttemptSnapshotTenant,
		ReadNativeAttemptRunTenant:           s.readNativeAttemptRunTenant,
		SummarizeChangeWithAuditDispositions: s.summarizeChangeWithAuditDispositions,
		HydrateChangeSummaryPolicy:           hydrateChangeSummaryPolicy,
		BoundedChangeVersion:                 boundedChangeVersion,
		ChangeDetailResponse:                 changeDetailResponse,
		ValidChangeReadSection:               validChangeReadSection,
		ChangeLimit:                          changeLimit,
		RecoveryRecordFragment:               recoveryRecordFragment,
		AppendBoundedChangeEvidence:          appendBoundedChangeEvidence,
	})
}

type changeWorkflowTransition struct {
	feature  *changelanding.WorkflowTransition
	eligible bool
	request  tracker.Transition
	outbound *projectstore.GitHubOutbound
	plan     *hubrecords.WorkItemRecordPlan
}

func rootChangeWorkflowTransition(p *changelanding.WorkflowTransition) *changeWorkflowTransition {
	if p == nil {
		return nil
	}
	return &changeWorkflowTransition{feature: p, eligible: p.Eligible, plan: &p.Plan, request: p.Request, outbound: p.Outbound}
}

type changeReviewWorkflow = changelanding.ReviewWorkflow

func (s *Service) prepareChangeReviewWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, review tracker.ChangeReview, approval hubrecords.ChangeVersionApproval, now time.Time) (*changeReviewWorkflow, error) {
	return s.changeWorkflow().PrepareChangeReviewWorkflow(ctx, scope, change, version, review, approval, now)
}

func (s *Service) prepareChangeCheckWorkflow(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, check tracker.ChangeCheck, approval hubrecords.ChangeVersionApproval, now time.Time) (*changeReviewWorkflow, error) {
	return s.changeWorkflow().PrepareChangeCheckWorkflow(ctx, scope, change, version, check, approval, now)
}

func (s *Service) recheckChangeReviewWorkflow(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeReviewWorkflow, current eventstore.Change) (bool, error) {
	return s.changeWorkflow().RecheckChangeReviewWorkflow(ctx, w, scope, p, current)
}

func (s *Service) prepareChangeWorkflowTransition(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, approval hubrecords.ChangeVersionApproval, rework bool, now time.Time) (*changeWorkflowTransition, error) {
	p, err := s.changeWorkflow().PrepareChangeWorkflowTransition(ctx, scope, change, approval, rework, now)
	return rootChangeWorkflowTransition(p), err
}

func (s *Service) lockChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeWorkflowTransition) (bool, error) {
	if p == nil {
		return false, nil
	}
	return s.changeWorkflow().LockChangeWorkflowTransition(ctx, w, scope, p.feature)
}

func (s *Service) applyChangeWorkflowTransition(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *changeWorkflowTransition) error {
	return s.changeWorkflow().ApplyChangeWorkflowTransition(ctx, w, scope, p.feature)
}

type staleLandingRoutes = changelanding.StaleLandingRoutes

func (s *Service) prepareStaleLandingRoutes(ctx context.Context, scope hubscope.NativeScope, approval hubrecords.ChangeVersionApproval, now time.Time) (*staleLandingRoutes, error) {
	return s.changeWorkflow().PrepareStaleLandingRoutes(ctx, scope, approval, now)
}

func (s *Service) applyStaleLandingRoutes(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, routes *staleLandingRoutes) error {
	return s.changeWorkflow().ApplyStaleLandingRoutes(ctx, w, scope, routes)
}

func (s *Service) changeWorkflow() *changelanding.Workflow {
	return changelanding.NewWorkflow(changelanding.WorkflowDeps{
		SettleLease:                            s.settleChangeLease,
		Repository:                             s.config.TenantRepository,
		Clock:                                  s.config.Clock,
		ConversationDispatch:                   s.config.ConversationDispatch,
		ReadWorkItemRecord:                     s.readWorkItemRecord,
		HydrateWorkItemRecord:                  s.hydrateWorkItemRecord,
		ReadProjectRecord:                      s.readProjectRecord,
		RunDetailStore:                         s.runDetailStore,
		NativeIssueResponse:                    s.nativeIssueResponse,
		PrepareCompatibilityWorkflowLabel:      s.prepareCompatibilityWorkflowLabel,
		ValidateTenantTransitionIssueContract:  s.validateTenantTransitionIssueContract,
		AuthorizeWorkItemRecordScope:           s.authorizeWorkItemRecordScope,
		AppendBoundedChangeEvidence:            appendBoundedChangeEvidence,
		PrepareValidatorAuditDispositions:      s.prepareValidatorAuditDispositions,
		CutoverSourceReference:                 cutoverSourceReference,
		ValidateConfiguredWorkflowPolicy:       validateConfiguredWorkflowPolicy,
		SameNativeClaimLinkedSource:            sameNativeClaimLinkedSource,
		SameTenantProjectPolicyRevisionContent: sameTenantProjectPolicyRevisionContent,
		ConversationDispatchUnavailable:        conversationDispatchUnavailable,
		PrepareLeaseCancellation:               s.prepareChangeLeaseCancellation,
	})
}

func (s *Service) prepareChangeLeaseCancellation(ctx context.Context, scope hubscope.NativeScope, item int64) (changelanding.LeaseCancellation, error) {
	prepared, err := s.prepareExpiredNativeLeaseCancellation(ctx, scope, item, false)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, lease leasestore.Lease, request tracker.Transition, stopState string, clock func() time.Time) error {
		if !prepared.expiresLease(lease) && lease.ExpiresAt.After(clock().UTC()) {
			return tracker.ErrLeaseConflict
		}
		return s.settleHumanWorkflowLease(ctx, w, scope, lease, request, prepared, stopState)
	}, nil
}

func (s *Service) settleChangeLease(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, lease leasestore.Lease, request tracker.Transition, prepared changelanding.LeaseCancellation, stopState string, clock func() time.Time) error {
	if prepared != nil {
		return prepared(ctx, w, scope, lease, request, stopState, clock)
	}
	if lease.ExpiresAt.After(clock().UTC()) {
		return tracker.ErrLeaseConflict
	}
	return s.settleHumanWorkflowLease(ctx, w, scope, lease, request, nil, stopState)
}
