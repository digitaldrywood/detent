package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/changeapi"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/changerules"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/releasefeed"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
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
		RecoverAuditHold:              s.recoverRecordedAuditHold,
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
		BoundReview:                          boundedChangeReviewResult,
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

func (s *Service) landChangeRecord(c echo.Context) error {
	return s.changeLanding().LandChangeRecord(c)
}

func (s *Service) changeLanding() *changelanding.Landing {
	return changelanding.NewLanding(changelanding.LandingDeps{
		Repository:              s.config.TenantRepository,
		CurrentTime:             s.database.currentTime,
		BoardRefresh:            s.boardRefresh,
		SourceVerification:      s.sourceVerification,
		RunDetailContext:        s.runDetailContext,
		RunDetailStore:          s.runDetailStore,
		NativeAPIError:          s.nativeAPIError,
		InvalidAPIRequest:       s.invalidAPIRequest,
		AuthorizeAccessScope:    s.authorizeAccessScope,
		ChangeRecordReceipt:     s.changeRecordReceipts().ChangeRecordReceipt,
		ReadChangeRecord:        s.readChangeRecord,
		ReadChangeVersionRecord: s.readChangeVersionRecord,
		ReadWorkItemRecord:      s.readWorkItemRecord,
		HydrateWorkItemRecord:   s.hydrateWorkItemRecord,
		RunnerSessionFence:      s.runnerSessionFence,
		SelectedChangeDetail:    s.selectedChangeDetail,
	})
}

func (s *Service) changeReviewPolicyRecord(ctx context.Context, scope hubscope.NativeScope) (tracker.ChangeReviewPolicy, error) {
	return s.changeReviewPolicy().ChangeReviewPolicyRecord(ctx, scope)
}

func (s *Service) currentChangePolicyRows(ctx context.Context, scope hubscope.NativeScope) (hubrecords.CurrentChangePolicyRows, error) {
	return s.changeReviewPolicy().CurrentChangePolicyRows(ctx, scope)
}

func (s *Service) approveChangeReviewPolicyRecord(c echo.Context, request tracker.ApproveChangeReviewPolicy) error {
	return s.changeReviewPolicy().ApproveChangeReviewPolicyRecord(c, request)
}

func (s *Service) approveChangeReviewPolicyRecordCommand(ctx context.Context, scope hubscope.NativeScope, operation string, request tracker.ApproveChangeReviewPolicy) (json.RawMessage, error) {
	return s.changeReviewPolicy().ApproveChangeReviewPolicyRecordCommand(ctx, scope, operation, request)
}

func (s *Service) changeReviewPolicy() *changerules.ReviewPolicy {
	return changerules.NewReviewPolicy(changerules.ReviewPolicyDeps{
		PrepareStaleLanding:          s.prepareStaleLandingForPolicy,
		Repository:                   s.config.TenantRepository,
		CurrentTime:                  s.database.currentTime,
		BoardRefresh:                 s.boardRefresh,
		RunDetailStore:               s.runDetailStore,
		RunDetailContext:             s.runDetailContext,
		NativeAPIError:               s.nativeAPIError,
		RecheckProjectRecordMutation: s.recheckProjectRecordMutation,
		AuthorizeAccessScope:         s.authorizeAccessScope,
		ChangeRecordReceipt:          s.changeRecordReceipt,

		HydrateChangeSummaryPolicy: hydrateChangeSummaryPolicy,
		FollowsRepositoryGate:      followsRepositoryGate,
	})
}

func (s *Service) prepareStaleLandingForPolicy(ctx context.Context, scope hubscope.NativeScope, approval hubrecords.ChangeVersionApproval, now time.Time) (changerules.ApplyStaleLanding, bool, error) {
	routes, err := s.prepareStaleLandingRoutes(ctx, scope, approval, now)
	if err != nil {
		return nil, false, err
	}
	if routes == nil {
		return nil, false, nil
	}
	return func(ctx context.Context, w tenantstore.Writer) error {
		return s.applyStaleLandingRoutes(ctx, w, scope, routes)
	}, routes.HasMoves(), nil
}

func landingCapacityKey(machine tracker.MachineID, project, session string) string {
	return changelanding.LandingCapacityKey(machine, project, session)
}

func (s *Service) changeCapacity() *changelanding.Capacity {
	return changelanding.NewCapacity(changelanding.CapacityDeps{
		Repository:                 s.config.TenantRepository,
		Clock:                      s.config.Clock,
		NoClaimableWork:            ErrNoClaimableWork,
		HydrateCurrentChangeDetail: s.hydrateCurrentChangeDetail,
	})
}

const nativeLandingCapacityUnknown = changelanding.CapacityUnknown

type nativeLandingRenewal struct {
	lease    leasestore.Lease
	prepared *changelanding.Renewal
}

func (s *Service) prepareNativeLandingRenewal(ctx context.Context, scope hubscope.NativeScope, leaseID string, capacity int) (*nativeLandingRenewal, error) {
	prepared, err := s.changeCapacity().PrepareRenewal(ctx, scope, leaseID, capacity)
	if prepared == nil || err != nil {
		return nil, err
	}
	return &nativeLandingRenewal{prepared: prepared}, nil
}

func (s *Service) reserveNativeLandingRenewal(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, p *nativeLandingRenewal, lease leasestore.Lease, capacity int64) (int64, error) {
	if p == nil {
		return capacity, nil
	}
	prepared := p.prepared
	if prepared == nil {
		prepared = changelanding.ExistingRenewal(p.lease)
	}
	return s.changeCapacity().ReserveRenewal(ctx, w, scope, prepared, lease, capacity)
}

func (s *Service) publishChangeVersion(c echo.Context) error {
	return s.changeVersionPublication().PublishChangeVersionRecord(c)
}

func (s *Service) publishChangeVersionRecordCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, operation string, request tracker.PublishChangeVersion) (json.RawMessage, error) {
	return s.changeVersionPublication().PublishChangeVersionRecordCommand(ctx, scope, item, changeID, operation, request)
}

func (s *Service) changeVersionPublication() *changeapi.VersionPublication {
	return changeapi.NewVersionPublication(changeapi.VersionPublicationDeps{
		RunEventsAvailable: func() bool { return s.config.NativeRunEvents != nil },
		ValidateRunOwner:   s.validateChangeVersionRunOwner,
		ExecutionConflict:  nativeExecutionConflict,
		Repository:         s.config.TenantRepository,
		CurrentTime:        s.database.currentTime,
		BoardRefresh:       s.boardRefresh,
		Authority:          s.config.ChangeVersionAuthority,
		DefaultAuthority: func() changeapi.VersionAuthority {
			return tenantChangeVersionAuthority{repository: s.config.TenantRepository, content: eventstore.NewContent(s.runDetailStore())}
		},

		ValidateVersionAttachments:      validateVersionAttachments,
		BindVersionAttachments:          bindVersionAttachments,
		RunDetailContext:                s.runDetailContext,
		RunDetailStore:                  s.runDetailStore,
		NativeAPIError:                  s.nativeAPIError,
		InvalidAPIRequest:               s.invalidAPIRequest,
		ChangeOperationRecordReceipt:    s.changeOperationRecordReceipt,
		ReadChangeRecord:                s.readChangeRecord,
		VerifyChangeSource:              s.verifyChangeSource,
		LockChangeRecordOperation:       s.lockChangeRecordOperation,
		PrepareChangeWorkflowTransition: s.changeWorkflow().PrepareChangeWorkflowTransition,
		LockChangeWorkflowTransition:    s.changeWorkflow().LockChangeWorkflowTransition,
		ApplyChangeWorkflowTransition:   s.changeWorkflow().ApplyChangeWorkflowTransition,
		LockRunnerCredential:            lockRunnerCredential,
		NativeRejection:                 nativeRejection,
		ValidateChangeVersion:           validateChangeVersion,
		ValidateChangeVersionEvidence:   validateChangeVersionEvidence,
	})
}

func validateVersionAttachments(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, artifacts []tracker.ChangeArtifact) error {
	_, err := validateChangeRecordContent(ctx, r, scope, nil, artifacts, "artifacts", false)
	return err
}

func bindVersionAttachments(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item string, artifacts []tracker.ChangeArtifact) error {
	return bindChangeRecordContent(ctx, w, scope, item, nil, artifacts, "artifacts")
}

func (s *Service) getReleaseDeliveryStatus(c echo.Context) error {
	return s.changeDelivery().GetReleaseDeliveryStatus(c)
}

func (s *Service) associateReleasePick(c echo.Context) error {
	return s.changeDelivery().AssociateReleasePick(c)
}

func readReleaseDeliveryContractFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID, states []tracker.NativeState) (hubrecords.ReleaseDeliveryContractFacts, error) {
	return releasefeed.ReadReleaseDeliveryContractFacts(ctx, r, scope, item, states)
}

func releaseDeliveryContractFactsCurrent(plan hubrecords.NativeClaimPlan, contract config.IssueContract) bool {
	return releasefeed.ReleaseDeliveryContractFactsCurrent(plan, contract)
}

func readNativeClaimReleaseDeliveryFacts(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, plans []hubrecords.NativeClaimPlan) error {
	return releasefeed.ReadNativeClaimReleaseDeliveryFacts(ctx, r, scope, plans)
}

func (s *Service) readReleaseDeliveryRecord(ctx context.Context, scope hubscope.NativeScope, item string) (tracker.ReleaseDelivery, bool, error) {
	return s.changeDelivery().ReadReleaseDeliveryRecord(ctx, scope, item)
}

func (s *Service) prepareReleasePublication(ctx context.Context, scope hubscope.NativeScope, item string, r tracker.PublishReleasePick) (tracker.ReleasePickPublication, error) {
	return s.changeDelivery().PrepareReleasePublication(ctx, scope, item, r)
}

func (s *Service) publishReleasePick(c echo.Context) error {
	return s.changeDelivery().PublishReleasePick(c)
}

func (s *Service) recordReleasePublicationCommand(ctx context.Context, scope hubscope.NativeScope, item string, request tracker.PublishReleasePick, result tracker.ReleasePickPublication) (json.RawMessage, error) {
	return s.changeDelivery().RecordReleasePublicationCommand(ctx, scope, item, request, result)
}

func (s *Service) associateReleasePickTenant(ctx context.Context, scope hubscope.NativeScope, item, operation string, request tracker.AssociateReleasePick) (json.RawMessage, error) {
	return s.changeDelivery().AssociateReleasePickTenant(ctx, scope, item, operation, request)
}

func (s *Service) changeDelivery() *releasefeed.Delivery {
	return releasefeed.NewDelivery(releasefeed.DeliveryDeps{
		Repository:                          s.config.TenantRepository,
		Clock:                               s.config.Clock,
		BoardRefresh:                        s.boardRefresh,
		RunDetailContext:                    s.runDetailContext,
		RunDetailStore:                      s.runDetailStore,
		InvalidAPIRequest:                   s.invalidAPIRequest,
		NativeAPIError:                      s.nativeAPIError,
		RecheckProjectRecordMutation:        s.recheckProjectRecordMutation,
		AuthorizeWorkItemRecordScope:        s.authorizeWorkItemRecordScope,
		ReadWorkItemRecord:                  s.readWorkItemRecord,
		HydrateWorkItemRecord:               s.hydrateWorkItemRecord,
		ReadChangeRecord:                    s.readChangeRecord,
		NativeIssueResponse:                 s.nativeIssueResponse,
		PrepareWorkItemFields:               s.prepareWorkItemFields,
		PrepareChangeWorkflowTransition:     s.changeWorkflow().PrepareChangeWorkflowTransition,
		LockChangeWorkflowTransition:        s.changeWorkflow().LockChangeWorkflowTransition,
		ApplyChangeWorkflowTransition:       s.changeWorkflow().ApplyChangeWorkflowTransition,
		HydrateNativeAttemptTenant:          s.hydrateNativeAttemptTenant,
		HydrateCurrentChangeVersionApproval: hydrateCurrentChangeVersionApproval,
		AnsweredClaimKind:                   answeredClaimKind,
		ApproveReleaseDeliveryContract:      approveReleaseDeliveryContract,
		ProjectRecordActor:                  projectRecordActor,
		ValidCheckoutRepository:             validCheckoutRepository,
		ValidateReleasePick:                 validateReleasePick,
		ValidatePreparedReleasePick:         validatePreparedReleasePick,
		SameReleasePickDelivery:             sameReleasePickDelivery,
		RequireCollaboration:                s.requireReleaseCollaboration,
		HasPlans:                            func() bool { return s.database.plansService().Plans() != nil },
		CheckoutRepository:                  s.releaseCheckoutRepository,
		InvalidatePullRequests: func(scope hubscope.NativeScope, item string) {
			s.pullRequests.invalidate(pullRequestCacheKey(scope, item))
		},
		DeliveryVerifier: func() (githubcontract.ReleaseDeliveryVerifier, bool) {
			v, ok := s.config.ReconcileBackend.(ReleaseDeliveryVerifier)
			return v, ok
		},
		PickVerifier: func() (githubcontract.ReleasePickVerifier, bool) {
			v, ok := s.config.ReconcileBackend.(ReleasePickVerifier)
			return v, ok
		},
		PickPublisher: func() (githubcontract.ReleasePickPublisher, bool) {
			v, ok := s.config.ReconcileBackend.(ReleasePickPublisher)
			return v, ok
		},
	})
}

func (s *Service) releaseCheckoutRepository(ctx context.Context, scope hubscope.NativeScope) (string, bool, error) {
	connector, _, _, err := s.projectRepositoryBinding(ctx, nil, scope)
	if err != nil || connector == nil {
		return "", false, err
	}
	return connector.Repository, true, nil
}

func (s *Service) requireReleaseCollaboration(ctx context.Context, r tenantstore.Reader, now time.Time) error {
	return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, r, "collaboration", now))
}

func prepareChangeContent(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	return changefiles.PrepareChangeContent(ctx, organization, data)
}

func prepareMeteredContent(ctx context.Context, organization string, data []byte, billable bool) (attachment.ArchiveReference, error) {
	return changefiles.PrepareMeteredContent(ctx, organization, data, billable)
}

func (s *Service) registerChangeContent(ctx context.Context, organization string, ref attachment.ArchiveReference, billable bool) error {
	return s.changeContent().RegisterChangeContent(ctx, organization, ref, billable)
}

func (s *Service) registerConfigurationContent(ctx context.Context, organization string, ref attachment.ArchiveReference) error {
	return s.changeContent().RegisterConfigurationContent(ctx, organization, ref)
}

func (s *Service) registerChangeContentRecord(ctx context.Context, w tenantstore.Writer, organization string, ref attachment.ArchiveReference, billable, withinAllowance bool) error {
	return s.changeContent().RegisterChangeContentRecord(ctx, w, organization, ref, billable, withinAllowance)
}

func (s *Service) checkChangeContentAllowance(ctx context.Context, organization string, refs map[string]attachment.ArchiveReference, next attachment.ArchiveReference) error {
	return s.changeContent().CheckChangeContentAllowance(ctx, organization, refs, next)
}

func (s *Service) changeContent() *changefiles.Content {
	return changefiles.NewContent(changefiles.ContentDeps{
		Repository: s.config.TenantRepository,
		Clock:      s.config.Clock,
		Plans:      func() *hostedplans.HostedPlansConfig { return s.database.plansService().Plans() },
		EntitlementRecord: func(ctx context.Context, r billingstore.Reader, now time.Time) (hostedplans.HostedEntitlement, error) {
			return s.database.plansService().EntitlementRecord(ctx, r, now)
		},
	})
}

func validateQualityBasis(snapshot *tracker.LandingQualitySnapshot, escape tracker.QualityEscape) error {
	return changerules.ValidateQualityBasis(snapshot, escape)
}

type preparedTenantQualityEscape = changerules.PreparedQualityEscape

func (s *Service) prepareQualityEscapeTenant(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.DiscussChange, now time.Time) (*preparedTenantQualityEscape, error) {
	return s.changeQuality().PrepareQualityEscapeTenant(ctx, scope, change, request, now)
}

func (s *Service) changeQuality() *changerules.QualityEscapes {
	return changerules.NewQualityEscapes(changerules.QualityEscapesDeps{
		PopulationLimit:                maxAnalyticsPopulation,
		Repository:                     s.config.TenantRepository,
		AuthorizeAccessScope:           s.authorizeAccessScope,
		RunDetailStoreFor:              s.runDetailStoreFor,
		HydrateCanonicalWorkItemRecord: s.hydrateCanonicalWorkItemRecord,
		HydrateCommentRecord:           s.hydrateCommentRecord,
	})
}

func validateChangeRecordContent(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, fields []cloudContentField, artifacts []tracker.ChangeArtifact, artifactName string, locked bool) ([]string, error) {
	return changefiles.ValidateRecordContent(ctx, r, scope, changeAttachmentFields(fields), artifacts, artifactName, locked, cloudContentAttachmentReferences)
}

func bindChangeRecordContent(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item string, fields []cloudContentField, artifacts []tracker.ChangeArtifact, artifactName string) error {
	return changefiles.BindRecordContent(ctx, w, scope, item, changeAttachmentFields(fields), artifacts, artifactName, cloudContentAttachmentReferences)
}

func changeAttachmentFields(fields []cloudContentField) []changefiles.ContentField {
	result := make([]changefiles.ContentField, len(fields))
	for i, field := range fields {
		result[i] = changefiles.ContentField{Name: field.name, Value: field.value}
	}
	return result
}

func (s *Service) hostedChangeCheckPrincipal(c echo.Context, credential hubapi.APICredential) bool {
	return s.changeHTTP().HostedChangeCheckPrincipal(c, credential)
}

func (s *Service) requireHostedChangeCheckPrincipal(ctx context.Context, scope hubscope.NativeScope) (eventstore.ReviewPolicy, error) {
	return s.changeHTTP().RequireHostedChangeCheckPrincipal(ctx, scope)
}

func (s *Service) recheckHostedChangeCheckPrincipal(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, approved eventstore.ReviewPolicy) error {
	return s.changeHTTP().RecheckHostedChangeCheckPrincipal(ctx, r, scope, approved)
}

func (s *Service) requireChangeReviewer() echo.MiddlewareFunc {
	return s.changeHTTP().RequireChangeReviewer()
}

func (s *Service) requireChangeReviewPolicyAdmin() echo.MiddlewareFunc {
	return s.changeHTTP().RequireChangeReviewPolicyAdmin()
}

func (s *Service) getChangeReviewPolicy(c echo.Context) error {
	return s.changeHTTP().GetChangeReviewPolicy(c)
}

func (s *Service) approveChangeReviewPolicy(c echo.Context) error {
	return s.changeHTTP().ApproveChangeReviewPolicy(c)
}

func (s *Service) listChanges(c echo.Context) error {
	return s.changeHTTP().ListChanges(c)
}

func (s *Service) readChanges(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	return s.changeHTTP().ReadChanges(ctx, scope, item)
}

func (s *Service) createChange(c echo.Context) error {
	return s.changeHTTP().CreateChange(c)
}

func (s *Service) getChange(c echo.Context) error {
	return s.changeHTTP().GetChange(c)
}

func changeDetailResponse(detail tracker.ChangeDetail) tracker.ChangeDetail {
	return changeapi.ChangeDetailResponse(detail)
}

func (s *Service) reviewChange(c echo.Context) error {
	return s.changeHTTP().ReviewChange(c)
}

func (s *Service) submitChangeCheck(c echo.Context) error {
	return s.changeHTTP().SubmitChangeCheck(c)
}

func (s *Service) discussChange(c echo.Context) error {
	return s.changeHTTP().DiscussChange(c)
}

func (s *Service) changeHTTP() *changeapi.HTTP {
	return changeapi.NewHTTP(changeapi.HTTPDeps{
		RunDetailContext:                s.runDetailContext,
		StorageArchiveContent:           s.storageArchiveContent,
		AuthorizeAccessScope:            s.authorizeAccessScope,
		AuthorizeWorkItemRecordScope:    s.authorizeWorkItemRecordScope,
		ReviewChangeRecordCommand:       s.reviewChangeRecordCommand,
		Repository:                      s.config.TenantRepository,
		Clock:                           s.config.Clock,
		NativeAPIError:                  s.nativeAPIError,
		InvalidAPIRequest:               s.invalidAPIRequest,
		RequireNativeScope:              s.requireNativeScope,
		RequireInstanceAdmin:            s.requireInstanceAdmin,
		RequireOnboardingAdmin:          s.requireOnboardingAdmin,
		RunDetailStore:                  s.runDetailStore,
		ChangeReviewPolicyRecord:        s.changeReviewPolicyRecord,
		ApproveChangeReviewPolicyRecord: s.approveChangeReviewPolicyRecord,
		ListChangeRecords:               s.listChangeRecords,
		CreateChangeRecord:              s.createChangeRecord,
		SelectedChangeDetail:            s.selectedChangeDetail,
		ReadBoundedChangeRecord:         s.readBoundedChangeRecord,
		ChangeRecordEvidenceMutation:    s.changeRecordEvidenceMutation,
		ChangeCheckRecordMutation:       s.changeCheckRecordMutation,
		SameNativeClaimReviewPolicy:     sameNativeClaimReviewPolicy,
		NativeArchiveJSON:               nativeArchiveJSON,
		ChangeCheckRequest:              changeCheckRequest,
		HostedOrganization:              s.changeHostedOrganization,
		ChangeResult:                    (hubChangeApplication{service: s}).result,
	})
}

func (s *Service) listChangeRecords(ctx context.Context, scope hubscope.NativeScope, item string) ([]tracker.ChangeRequest, error) {
	return s.changeRecordReader().ListChangeRecords(ctx, scope, item)
}

func (s *Service) filterChangeRecord(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
	return s.changeRecordReader().FilterChangeRecord(ctx, scope, change)
}

func (s *Service) readChangeRecord(ctx context.Context, scope hubscope.NativeScope, item, id string) (tracker.ChangeRequest, error) {
	return s.changeRecordReader().ReadChangeRecord(ctx, scope, item, id)
}

func (s *Service) readChangeVersionRecord(ctx context.Context, organization tracker.OrganizationID, changeID, versionID string) (eventstore.Version, error) {
	return s.changeRecordReader().ReadChangeVersionRecord(ctx, organization, changeID, versionID)
}

func (s *Service) selectedChangeDetail(ctx context.Context, scope hubscope.NativeScope, item, id string, now time.Time) (tracker.ChangeDetail, error) {
	return s.changeRecordReader().SelectedChangeDetail(ctx, scope, item, id, now)
}

func (s *Service) nativeClaimCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.NativeClaimPlan, now time.Time) (tracker.ChangeDetail, error) {
	return s.changeRecordReader().NativeClaimCurrentChangeDetail(ctx, scope, plan, now)
}

func (s *Service) selectedCurrentChangeSummary(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, id string, now time.Time) (tracker.ChangeSummary, error) {
	return s.changeRecordReader().SelectedCurrentChangeSummary(ctx, scope, item, id, now)
}

func (s *Service) hydrateCurrentChangeDetail(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time) (tracker.ChangeDetail, error) {
	return s.changeRecordReader().HydrateCurrentChangeDetail(ctx, scope, item, changeRecord, versionRecord, policyRows, now)
}

func (s *Service) hydrateCurrentChangeDetailWithFacts(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload) (tracker.ChangeDetail, error) {
	return s.changeRecordReader().HydrateCurrentChangeDetailWithFacts(ctx, scope, item, changeRecord, versionRecord, policyRows, now, facts, approval, workload)
}

func (s *Service) hydrateCurrentChangeDetailSelection(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, changeRecord eventstore.Change, versionRecord *eventstore.Version, policyRows hubrecords.CurrentChangePolicyRows, now time.Time, facts *tenantstore.BoardChangeRows, approval *hubrecords.ChangeVersionApproval, workload corestore.Workload, recovery bool) (tracker.ChangeDetail, error) {
	return s.changeRecordReader().HydrateCurrentChangeDetailSelection(ctx, scope, item, changeRecord, versionRecord, policyRows, now, facts, approval, workload, recovery)
}

func (s *Service) selectedLatestChangeSummary(ctx context.Context, scope hubscope.NativeScope, item string, now time.Time) (tracker.NativeChangeReference, tracker.ChangeSummary, bool, error) {
	return s.changeRecordReader().SelectedLatestChangeSummary(ctx, scope, item, now)
}

func (s *Service) loadChangeExternalRecord(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, detail *tracker.ChangeDetail) error {
	return s.changeRecordReader().LoadChangeExternalRecord(ctx, scope, item, detail)
}

func (s *Service) changeRecordReader() *changeapi.RecordReader {
	return changeapi.NewRecordReader(changeapi.RecordReaderDeps{
		Repository:                              s.config.TenantRepository,
		RunDetailStore:                          s.runDetailStore,
		ReadWorkItemRecord:                      s.readWorkItemRecord,
		HydrateWorkItemRecord:                   s.hydrateWorkItemRecord,
		HydrateChangeVersion:                    s.hydrateChangeVersion,
		HydrateChangeVersionWithRows:            s.hydrateChangeVersionWithRows,
		RunDetailStoreFor:                       s.runDetailStoreFor,
		SummarizeChangeWithAuditDispositions:    s.summarizeChangeWithAuditDispositions,
		SummarizeChangeWithAuditDispositionsFor: s.summarizeChangeWithAuditDispositionsFor,
		ReadBoundedChangePushes:                 s.readBoundedChangePushes,
		HydrateChangeSummaryPolicy:              hydrateChangeSummaryPolicy,
		BoardCurrentApproval:                    boardCurrentApproval,
		FilterBoardChangeRecord:                 filterBoardChangeRecord,

		ExternalPullRequestNumber: externalPullRequestNumber,
		SplitRepositoryFullName:   splitRepositoryFullName,
	})
}

func (s *Service) changeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	return s.changeRecordReceipts().ChangeRecordReceipt(ctx, scope, operation, key, hash)
}

func (s *Service) authorizedChangeRecordReceipt(ctx context.Context, scope hubscope.NativeScope, operation, key, hash string) (json.RawMessage, bool, error) {
	return s.changeRecordReceipts().AuthorizedChangeRecordReceipt(ctx, scope, operation, key, hash)
}

func (s *Service) lockChangeRecordOperation(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item, changeID, versionID string) (eventstore.Change, error) {
	return s.changeRecordReceipts().LockChangeRecordOperation(ctx, w, scope, item, changeID, versionID)
}

func (s *Service) changeOperationRecordReceipt(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation, key, hash string) (json.RawMessage, bool, error) {
	return s.changeRecordReceipts().ChangeOperationRecordReceipt(ctx, scope, item, changeID, versionID, operation, key, hash)
}

func (s *Service) changeRecordReceipts() *changeapi.RecordReceipts {
	return changeapi.NewRecordReceipts(changeapi.RecordReceiptsDeps{
		Repository:                   s.config.TenantRepository,
		Clock:                        s.config.Clock,
		RunDetailStore:               s.runDetailStore,
		AuthorizeAccessScope:         s.authorizeAccessScope,
		AuthorizeWorkItemRecordScope: s.authorizeWorkItemRecordScope,
		HasAccessRepository:          s.config.AccessRepository != nil,
	})
}

func (s *Service) changeRecordEvidenceMutation(c echo.Context, kind, versionID, sourceKey string, value any) error {
	return s.changeRecordMutations().ChangeRecordEvidenceMutation(c, kind, versionID, sourceKey, value)
}

func (s *Service) changeRecordEvidenceCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, operation, kind, versionID, sourceKey string, value any) (json.RawMessage, error) {
	return s.changeRecordMutations().ChangeRecordEvidenceCommand(ctx, scope, item, changeID, operation, kind, versionID, sourceKey, value)
}

func (s *Service) changeCheckRecordMutation(c echo.Context, request tracker.SubmitChangeCheck) error {
	return s.changeRecordMutations().ChangeCheckRecordMutation(c, request)
}

func (s *Service) createChangeRecord(c echo.Context, request tracker.CreateChange) error {
	return s.changeRecordMutations().CreateChangeRecord(c, request)
}

func (s *Service) createChangeRecordCommand(ctx context.Context, scope hubscope.NativeScope, itemID, operation string, request tracker.CreateChange) (json.RawMessage, error) {
	return s.changeRecordMutations().CreateChangeRecordCommand(ctx, scope, itemID, operation, request)
}

func (s *Service) changeRecordMutations() *changeapi.RecordMutations {
	return changeapi.NewRecordMutations(changeapi.RecordMutationsDeps{
		Repository:                        s.config.TenantRepository,
		Clock:                             s.config.Clock,
		BoardRefresh:                      s.boardRefresh,
		CurrentTime:                       s.database.currentTime,
		RunDetailContext:                  s.runDetailContext,
		RunDetailStore:                    s.runDetailStore,
		NativeAPIError:                    s.nativeAPIError,
		ReadChangeRecord:                  s.readChangeRecord,
		ReadChangeVersionRecord:           s.readChangeVersionRecord,
		ChangeRecordReceipt:               s.changeRecordReceipt,
		ReadWorkItemRecord:                s.readWorkItemRecord,
		AuthorizeAccessScope:              s.authorizeAccessScope,
		AuthorizeWorkItemRecordScope:      s.authorizeWorkItemRecordScope,
		RunnerSessionFence:                s.runnerSessionFence,
		RecheckHostedChangeCheckPrincipal: s.recheckHostedChangeCheckPrincipal,
		RequireHostedChangeCheckPrincipal: s.requireHostedChangeCheckPrincipal,
		ChangeReviewRecordMutation:        s.changeReviewRecordMutation,
		HydrateChangeSummaryPolicy:        hydrateChangeSummaryPolicy,
		ValidateNativeProvenance:          validateNativeProvenance,
		PrepareQualityEscapeTenant:        s.prepareQualityEscapeTenant,
		PrepareChangeCheckWorkflow:        s.changeWorkflow().PrepareChangeCheckWorkflow,
		RecheckChangeReviewWorkflow:       s.changeWorkflow().RecheckChangeReviewWorkflow,
		LockChangeWorkflowTransition:      s.changeWorkflow().LockChangeWorkflowTransition,
		ApplyChangeWorkflowTransition:     s.changeWorkflow().ApplyChangeWorkflowTransition,
		Hosted:                            s.config.Hosted != nil,
		InvalidClock:                      ErrInvalidClock,
		HasPlans:                          func() bool { return s.database.plansService().Plans() != nil },
		RequireCollaboration:              s.requireReleaseCollaboration,
		AttachmentReferences:              cloudContentAttachmentReferences,
	})
}

func appendBoundedChangeEvidence(detail *tracker.ChangeDetail, row eventstore.Evidence, raw []byte) error {
	return changeapi.AppendBoundedChangeEvidence(detail, row, raw, boundedChangeReviewResult)
}

func (s *Service) changeValidatorDeps() changeapi.ValidatorDeps {
	return changeapi.ValidatorDeps{
		Repository:                        s.config.TenantRepository,
		RunDetailStoreFor:                 s.runDetailStoreFor,
		RunnerSessionFence:                s.runnerSessionFence,
		ReadRunnerSessionPayload:          s.readRunnerSessionPayload,
		VerifyRunnerSessionFinalization:   s.verifyRunnerSessionFinalization,
		HydrateWorkItemRecord:             s.hydrateWorkItemRecord,
		PrepareValidatorAuditDispositions: s.prepareValidatorAuditDispositions,
		NativeRejection:                   nativeRejection,
		SecurityAuditRejection:            securityAuditRejection,
		RunnerSessionFenceWithOwner:       s.reviewRunnerSessionFence,
	}
}

func (s *Service) changeReviewRecordMutation(c echo.Context, request tracker.ReviewChange) error {
	return s.changeReview().ChangeReviewRecordMutation(c, request)
}

func (s *Service) reviewChangeRecordCommand(ctx context.Context, scope hubscope.NativeScope, item, changeID, versionID, operation string, request tracker.ReviewChange) (json.RawMessage, error) {
	return s.changeReview().ReviewChangeRecordCommand(ctx, scope, item, changeID, versionID, operation, request)
}

func (s *Service) changeReview() *changeapi.Review {
	workflow := s.changeWorkflow()
	return changeapi.NewReview(changeapi.ReviewDeps{
		Repository:                          s.config.TenantRepository,
		CurrentTime:                         s.database.currentTime,
		BoardRefresh:                        s.boardRefresh,
		RunDetailContext:                    s.runDetailContext,
		RunDetailStore:                      s.runDetailStore,
		NativeAPIError:                      s.nativeAPIError,
		ChangeOperationRecordReceipt:        s.changeOperationRecordReceipt,
		ReadChangeRecord:                    s.readChangeRecord,
		ReadChangeVersionRecord:             s.readChangeVersionRecord,
		SelectedCandidateChangeSummary:      s.changeRecordReader().SelectedCandidateChangeSummary,
		ReadWorkItemRecord:                  s.readWorkItemRecord,
		HydrateWorkItemRecord:               s.hydrateWorkItemRecord,
		LockChangeRecordOperation:           s.lockChangeRecordOperation,
		ValidateReviewBundleRecord:          s.validateReviewBundleRecord,
		RegisterChangeContentRecord:         s.registerChangeContentRecord,
		CheckChangeContentAllowance:         s.checkChangeContentAllowance,
		HydrateCurrentChangeVersionApproval: hydrateCurrentChangeVersionApproval,
		NativeRejection:                     nativeRejection,
		PrepareChangeReviewWorkflow:         workflow.PrepareChangeReviewWorkflow,
		RecheckChangeReviewWorkflow:         workflow.RecheckChangeReviewWorkflow,
		LockChangeWorkflowTransition:        workflow.LockChangeWorkflowTransition,
		ApplyChangeWorkflowTransition:       workflow.ApplyChangeWorkflowTransition,
		Validator:                           s.changeValidatorDeps(),
		AttachmentReferences:                cloudContentAttachmentReferences,
		PrepareRepairs:                      s.prepareChangeReviewRepairs,
	})
}

func (s *Service) prepareChangeReviewRepairs(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, failures []failureattribution.Evidence, source string, now time.Time) (*changeapi.PreparedRepairs, error) {
	plan, err := s.prepareIndependentRepairs(ctx, scope, item, failures, source, now)
	if err != nil || plan == nil {
		return nil, err
	}
	return &changeapi.PreparedRepairs{
		Dependencies: plan.dependencies,
		Refusal:      plan.refusal,
		Check: func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) (string, error) {
			return s.checkIndependentRepairs(ctx, w, scope, plan)
		},
		Apply: func(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope) error {
			return s.applyIndependentRepairs(ctx, w, scope, plan)
		},
	}, nil
}

func (s *Service) reviewRunnerSessionFence(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, request runnersession.Mutation, issue string, owner runnerstore.Identity) (leasestore.Attempt, corestore.WorkItem, policystore.Revision, error) {
	return s.runnerSessionFenceHeld(ctx, r, scope, request, issue, true, &heldRunnerSessionAuthority{owner: owner})
}

func nativeRuntimeSession(runtime *tracker.NativeRuntimeObservation) int64 {
	return changeapi.NativeRuntimeSession(runtime)
}

type preparedChangeVersionRun = changeapi.PreparedVersionRun

func (s *Service) prepareChangeVersionRun(ctx context.Context, scope hubscope.NativeScope, change tracker.ChangeRequest, request tracker.PublishChangeVersion) (*preparedChangeVersionRun, error) {
	return s.changeVersionPublication().PrepareChangeVersionRun(ctx, scope, change, request)
}

func (s *Service) validateChangeVersionRunOwner(ctx context.Context, r tenantstore.Reader, run changeapi.VersionRunAuthority) error {
	authority := NativeRunEventAuthority{
		Scope:              run.Scope,
		OrganizationID:     run.OrganizationID,
		ProjectID:          run.ProjectID,
		WorkItem:           run.WorkItem,
		Lease:              run.Lease,
		DispatchGeneration: run.DispatchGeneration,
		UsageCorrection:    run.UsageCorrection,
		Event:              run.Event,
	}
	return (tenantNativeRunEventParticipant{service: s}).validateNativeRunEventOwner(ctx, r, &authority)
}
