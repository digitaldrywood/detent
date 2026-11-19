package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changeapi"
	"github.com/digitaldrywood/detent/internal/hubserver/changelanding"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
