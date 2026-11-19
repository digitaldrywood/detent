package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/changeapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
