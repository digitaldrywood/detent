package hubserver

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) chargeProjectCommand(ctx context.Context, writer tenantstore.Writer, scope hubscope.NativeScope, operation, key string) error {
	if strings.TrimSpace(key) == "" {
		key = hubapi.NewID("mutation")
	}
	return s.database.chargeAttachmentAPIMutationsPostgres(ctx, writer, scope.Organization, "project-command/"+scope.Credential.ID+"/"+operation+"/"+key, s.config.Clock(), "command")
}

func (d *database) chargeAttachmentAPIMutationsPostgres(ctx context.Context, writer tenantstore.Writer, organization tracker.OrganizationID, receiptID string, now time.Time, charges ...string) error {
	return d.plansService().ChargeAttachmentAPIMutations(ctx, writer, organization, receiptID, now, ctx.Value(runnerRequestBudgetContext{}) == true, charges...)
}
