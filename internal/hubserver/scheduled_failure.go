package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) attributeScheduledFailure(ctx context.Context, scope hubscope.NativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	deps := operatorcommands.ScheduledAttributionDeps{
		Repository: s.config.TenantRepository, Importer: s.config.ImportBackend,
		Details: s.runDetailStore, Now: s.config.Clock, Operation: nativeOperation,
		RecheckProject: s.recheckProjectRecordMutation, Receipt: s.authorizedChangeRecordReceipt,
		LoadWorkItemPlan: s.loadWorkItemPlan, PlanWorkItemRecord: s.planWorkItemRecord,
		PrepareFields: s.prepareWorkItemFields, AuthorizeWorkItem: s.authorizeWorkItemRecordScope,
		ProjectActor: projectRecordActor,
		RequireCollaboration: func(ctx context.Context, w tenantstore.Writer, now time.Time) error {
			if s.config.Hosted == nil {
				return nil
			}
			return hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, w, "collaboration", now))
		},
	}
	return deps.Attribute(ctx, scope, request)
}
