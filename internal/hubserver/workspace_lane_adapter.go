package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/localboard"
)

func (s *Service) localBoardDeps() coreimport.LocalBoardDeps {
	return coreimport.LocalBoardDeps{
		Repository:                    s.config.TenantRepository,
		Clock:                         s.config.Clock,
		Plans:                         s.database.plansService,
		InvalidAPIRequest:             s.invalidAPIRequest,
		NativeAPIError:                s.nativeAPIError,
		AuthorizeWorkItemRecordScope:  s.authorizeWorkItemRecordScope,
		HydrateWorkItemRecord:         s.hydrateWorkItemRecord,
		PlanWorkItemRecord:            s.planWorkItemRecord,
		PrepareWorkItemFields:         s.prepareWorkItemFields,
		RunDetailStore:                s.runDetailStore,
		RecheckProjectRecordMutation:  s.recheckProjectRecordMutation,
		BeginProjectRecordAuthority:   s.beginProjectRecordAuthority,
		RecordEntryAccessKeyAuditWith: s.recordEntryAccessKeyAuditWith,
		ValidateWorkItemCreate:        validateWorkItemRecordCreateInput,
		ValidateLinkedRepository: func(record projectstore.Record, repository string) error {
			return validateLinkedIssueRepository(projectIntegrationRecord(record), repository)
		},
		IsRunnerBudget: func(ctx context.Context) bool {
			return ctx.Value(runnerRequestBudgetContext{}) == true
		},
		NotifyBoard: func() {
			select {
			case s.boardRefresh <- struct{}{}:
			default:
			}
		},
	}
}

func (s *Service) importLocalBoard(c echo.Context) error {
	return s.localBoardDeps().ImportLocalBoard(c)
}

func (s *Service) importLocalBoardRecord(ctx context.Context, scope hubscope.NativeScope, request localboard.Request) (localboard.Result, error) {
	return s.localBoardDeps().ImportLocalBoardRecord(ctx, scope, request)
}
