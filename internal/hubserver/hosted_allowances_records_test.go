package hubserver

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedConsumptionReadsTypedCollaborationBytesAndUsageWindows(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	organization := tracker.OrganizationID("org_consumption_records")
	plan := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 100, "api_mutations": 10}
	seedConversationAllowancePostgres(t, repository, organization, plan, allowances)
	if _, err := admin.ExecContext(t.Context(), `UPDATE hosted_usage_counters SET value=73 WHERE organization_id=$1 AND name='collaboration_bytes'`, organization); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(120, 0).UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: organization, ID: "mutation_example", WindowStart: now, Metric: "api_mutations", Amount: 4}, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	database := conversationAllowanceDatabase(organization, plan, allowances, repository)
	usage, err := database.hostedConsumption(t.Context(), nil, now, "collaboration_bytes", "usage_windows")
	if err != nil {
		t.Fatal(err)
	}
	if usage["collaboration_bytes"] != 73 || usage["api_mutations"] != 4 {
		t.Fatalf("typed hosted consumption=%v", usage)
	}
	usage, err = database.hostedConsumption(t.Context(), nil, now, "collaboration_bytes")
	if err != nil || len(usage) != 1 || usage["collaboration_bytes"] != 73 {
		t.Fatalf("collaboration-only consumption=%v err=%v", usage, err)
	}
	if _, err := database.hostedConsumption(t.Context(), nil, now, "history_records"); err != ErrBillingUsageUnavailable {
		t.Fatalf("unsupported PostgreSQL consumption error=%v", err)
	}
	if _, err := database.hostedConsumption(t.Context(), nil, now); err != ErrBillingUsageUnavailable {
		t.Fatalf("unbounded PostgreSQL consumption error=%v", err)
	}
}

func TestHostedConsumptionTreatsMissingCollaborationCounterAsUnavailable(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 3)
	organization := tracker.OrganizationID("org_missing_consumption")
	plan := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 100, "api_mutations": 10}
	seedConversationAllowancePostgres(t, repository, organization, plan, allowances)
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM hosted_usage_counters WHERE organization_id=$1 AND name='collaboration_bytes'`, organization); err != nil {
		t.Fatal(err)
	}
	database := conversationAllowanceDatabase(organization, plan, allowances, repository)
	_, err := database.hostedConsumption(t.Context(), nil, time.Unix(120, 0).UTC(), "collaboration_bytes")
	if err != ErrBillingUsageUnavailable {
		t.Fatalf("missing collaboration counter error=%v", err)
	}
}
