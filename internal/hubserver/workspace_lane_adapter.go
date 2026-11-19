package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/storageconversion"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/tracker"
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

const backgroundStorageBudget = storageconversion.BackgroundBudget
const storageConversionRows = storageconversion.MaxRows

func (s *Service) convertPostgresStorage(ctx context.Context, tenant hubmaintenance.Tenant, limit int) (int, error) {
	return storageconversion.ConvertTenant(ctx, tenant, limit, func(organization tracker.OrganizationID) storageconversion.Storage {
		return postgres.NewConversionStorage(s.database.postgres, organization, s.runDetailStore(), func(ctx context.Context, _ storageconversion.Candidate, raw []byte) ([]byte, error) {
			return hubcontent.ResolveStorageJSON(s.freshRunDetailContext(ctx), raw)
		})
	})
}
