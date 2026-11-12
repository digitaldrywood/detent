package hubserver

import (
	"fmt"
	"slices"
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
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
		for index := range 65 {
			name := fmt.Sprintf("unknown_metric_%02d", index)
			if _, err := writer.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: organization, ID: name, WindowStart: now, Metric: name, Amount: 1}, 10); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	database := conversationAllowanceDatabase(organization, plan, allowances, repository)
	usage, err := database.hostedConsumption(t.Context(), nil, now, "collaboration_bytes", "usage_windows")
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("unknown posted metrics leaked into selected consumption=%v", usage)
	}
	if _, err := database.hostedConsumption(t.Context(), nil, now, "unknown_metric_00"); err != ErrBillingUsageUnavailable {
		t.Fatalf("unsupported requested metric error=%v", err)
	}
	if usage["collaboration_bytes"] != 73 || usage["api_mutations"] != 4 {
		t.Fatalf("typed hosted consumption=%v", usage)
	}
	usage, err = database.hostedConsumption(t.Context(), nil, now, "collaboration_bytes")
	if err != nil || len(usage) != 1 || usage["collaboration_bytes"] != 73 {
		t.Fatalf("collaboration-only consumption=%v err=%v", usage, err)
	}
	usage, err = database.hostedConsumption(t.Context(), nil, now, "history_records", "events_total")
	if err != nil || len(usage) != 2 || usage["history_records"] != 0 || usage["events_total"] != 0 {
		t.Fatalf("initialized retained consumption=%v error=%v", usage, err)
	}
	runtime := *database.hostedRuntime.Load()
	runtime.Plans.ConnectedSeconds = 60
	database.hostedRuntime.Store(&runtime)
	var full hostedConsumptionRecord
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		full, err = database.readHostedConsumptionRecord(t.Context(), reader, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if full.usage["collaboration_bytes"] != 73 || full.usage["api_mutations"] != 4 || full.usage["history_records"] != 0 || !slices.Contains(full.unavailable, "artifact_retained_bytes") || !slices.Contains(full.unavailable, "artifact_reserved_bytes") {
		t.Fatalf("full typed consumption=%+v", full)
	}
	if _, err := database.hostedConsumption(t.Context(), nil, now); err != ErrBillingUsageUnavailable {
		t.Fatalf("unobserved artifact consumption error=%v", err)
	}

	second := tracker.OrganizationID("org_consumption_second")
	seedConversationAllowancePostgres(t, repository, second, plan, allowances)
	if err := repository.WriteTenant(t.Context(), second, corestore.Serving, func(writer tenantstore.Writer) error {
		_, err := writer.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: second, ID: "mutation_example", WindowStart: now, Metric: "api_mutations", Amount: 5}, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	secondDatabase := conversationAllowanceDatabase(second, plan, allowances, repository)
	usage, err = secondDatabase.hostedConsumption(t.Context(), nil, now, "collaboration_bytes", "history_records", "events_total", "usage_windows")
	if err != nil || usage["collaboration_bytes"] != 0 || usage["history_records"] != 0 || usage["events_total"] != 0 || usage["api_mutations"] != 5 {
		t.Fatalf("second tenant consumption=%v error=%v", usage, err)
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
