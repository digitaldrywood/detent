package hubserver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationCollaborationGrowthAllowancePostgres(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	now := time.Unix(120, 0).UTC()
	window := time.Unix(120, 0).UTC()
	tests := []struct {
		name              string
		allowances        map[string]int64
		before            int64
		after             int64
		completion        bool
		repeats           int
		additionalReceipt bool
		wantLimit         string
		wantAPIUsage      int64
	}{
		{name: "growth beyond allowance is rejected", allowances: map[string]int64{"collaboration_bytes": 5, "api_mutations": 10}, before: 4, after: 6, wantLimit: "collaboration_bytes"},
		{name: "no growth preserves existing overage", allowances: map[string]int64{"collaboration_bytes": 5, "api_mutations": 10}, before: 6, after: 6, wantAPIUsage: 1},
		{name: "completion bypasses growth and API limits", allowances: map[string]int64{"collaboration_bytes": 5, "api_mutations": 0}, before: 5, after: 9, completion: true, wantAPIUsage: 1},
		{name: "API mutation receipt replay counts once", allowances: map[string]int64{"collaboration_bytes": 10, "api_mutations": 2}, repeats: 2, additionalReceipt: true, wantAPIUsage: 2},
		{name: "API mutation quota rejects new receipt", allowances: map[string]int64{"collaboration_bytes": 10, "api_mutations": 0}, wantLimit: "api_mutations"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			organization := tracker.OrganizationID(fmt.Sprintf("org_allowance_%d", index+1))
			seedConversationAllowancePostgres(t, repository, organization, ref, test.allowances)
			database := conversationAllowanceDatabase(organization, ref, test.allowances, repository)
			repeats := test.repeats
			if repeats == 0 {
				repeats = 1
			}
			var gotLimit string
			for range repeats {
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
					return database.checkConversationCollaborationGrowthPostgres(t.Context(), writer, "receipt_example", test.before, test.after, now, test.completion)
				})
				var limit *hostedplans.LimitError
				if errors.As(err, &limit) {
					gotLimit = limit.Resource
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if test.additionalReceipt {
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
					return database.checkConversationCollaborationGrowthPostgres(t.Context(), writer, "receipt_second", 0, 0, now, false)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if gotLimit != test.wantLimit {
				t.Fatalf("limit resource=%q want=%q", gotLimit, test.wantLimit)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
				used, err := conversationAllowanceUsage(t.Context(), reader, window)
				if err != nil {
					return err
				}
				if used != test.wantAPIUsage {
					return fmt.Errorf("API mutation usage=%d want=%d", used, test.wantAPIUsage)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConversationCollaborationGrowthAllowancePostgresIsTenantScoped(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 10, "api_mutations": 2}
	first := tracker.OrganizationID("org_allowance_first")
	second := tracker.OrganizationID("org_allowance_second")
	seedConversationAllowancePostgres(t, repository, first, ref, allowances)
	seedConversationAllowancePostgres(t, repository, second, ref, allowances)
	database := conversationAllowanceDatabase(first, ref, allowances, repository)
	now := time.Unix(120, 0).UTC()
	err := repository.WriteTenant(t.Context(), second, corestore.Serving, func(writer tenantstore.Writer) error {
		return database.checkConversationCollaborationGrowthPostgres(t.Context(), writer, "receipt_cross_tenant", 0, 0, now, false)
	})
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("cross-tenant allowance write=%v", err)
	}
	for _, organization := range []tracker.OrganizationID{first, second} {
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			used, err := conversationAllowanceUsage(t.Context(), reader, now)
			if err != nil {
				return err
			}
			if used != 0 {
				return fmt.Errorf("organization %s usage=%d after cross-tenant write", organization, used)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func conversationAllowanceUsage(ctx context.Context, reader billingstore.Reader, window time.Time) (int64, error) {
	used, err := reader.BillingUsage(ctx, window, "api_mutations")
	if errors.Is(err, corestore.ErrNotFound) {
		return 0, nil
	}
	return used, err
}

func seedConversationAllowancePostgres(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, ref billingstore.PlanReference, allowances map[string]int64) {
	t.Helper()
	now := time.Unix(120, 0).UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
		if err := writer.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
			return err
		}
		if err := writer.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization, PlanReference: ref, Name: "Example", Features: []string{}, Allowances: allowances}); err != nil {
			return err
		}
		return writer.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: ref}, 0)
	}); err != nil {
		t.Fatal(err)
	}
}

func conversationAllowanceDatabase(organization tracker.OrganizationID, ref billingstore.PlanReference, allowances map[string]int64, repository tenantstore.Repository) *database {
	plan := hostedplans.HostedPlan{PlanReference: hostedplans.PlanReference{ID: ref.ID, Version: ref.Version}, Name: "Example", Features: []string{}, Allowances: allowances}
	database := &database{billingRepository: repository, hostedOrganization: organization}
	database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{Base: plan.PlanReference, Plans: []hostedplans.HostedPlan{plan}, WindowSeconds: 60}})
	return database
}

func TestAttachmentAPIMutationChargesPostgres(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"collaboration_bytes": 10, "api_mutations": 3}
	now := time.Unix(120, 0).UTC()
	organizations := []tracker.OrganizationID{"org_attachment_first", "org_attachment_second"}
	for _, organization := range organizations {
		seedConversationAllowancePostgres(t, repository, organization, ref, allowances)
	}
	charge := func(organization tracker.OrganizationID, receiptID string) error {
		database := conversationAllowanceDatabase(organization, ref, allowances, repository)
		return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			return database.chargeAttachmentAPIMutationsPostgres(t.Context(), writer, receiptID, now, "record", "mutation")
		})
	}
	for _, step := range []struct {
		organization tracker.OrganizationID
		receiptID    string
		wantRefused  int64
		wantUsage    int64
	}{
		{organizations[0], "upload_example", -1, 2},
		{organizations[0], "upload_example", -1, 2},
		{organizations[0], "upload_second", 3, 2},
		{organizations[1], "upload_example", -1, 2},
	} {
		err := charge(step.organization, step.receiptID)
		var limit *hostedplans.LimitError
		switch {
		case step.wantRefused < 0 && err != nil:
			t.Fatalf("%s %s charge=%v", step.organization, step.receiptID, err)
		case step.wantRefused >= 0 && (!errors.As(err, &limit) || limit.Resource != "api_mutations" || limit.Consumption != step.wantRefused):
			t.Fatalf("%s %s refusal=%v", step.organization, step.receiptID, err)
		}
		if err := repository.ReadTenant(t.Context(), step.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			used, err := conversationAllowanceUsage(t.Context(), reader, now)
			if err != nil {
				return err
			}
			if used != step.wantUsage {
				return fmt.Errorf("organization %s usage=%d want=%d", step.organization, used, step.wantUsage)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunEventGrowthCountsCompletionEventsPostgres(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	ref := billingstore.PlanReference{ID: "example", Version: 1}
	allowances := map[string]int64{"ingested_events": 0, "history_records": 0}
	now := time.Unix(120, 0).UTC()
	for _, test := range []struct {
		organization tracker.OrganizationID
		completion   bool
		wantLimit    string
		wantEvents   int64
	}{
		{organization: "org_completion_events", completion: true, wantEvents: 2},
		{organization: "org_observation_events", wantLimit: "history_records"},
	} {
		seedConversationAllowancePostgres(t, repository, test.organization, ref, allowances)
		service := &Service{database: conversationAllowanceDatabase(test.organization, ref, allowances, repository), config: Config{now: func() time.Time { return now }}}
		err := repository.WriteTenant(t.Context(), test.organization, corestore.Serving, func(writer tenantstore.Writer) error {
			growth, err := service.prepareRunEventGrowth(t.Context(), writer, test.completion)
			if err != nil {
				return err
			}
			growth.historyKnown = true
			return growth.apply(t.Context(), writer, test.organization, "run-event/example", 2)
		})
		var limit *hostedplans.LimitError
		if test.wantLimit != "" && (!errors.As(err, &limit) || limit.Resource != test.wantLimit) || test.wantLimit == "" && err != nil {
			t.Fatalf("%s growth error=%v want limit %q", test.organization, err, test.wantLimit)
		}
		if err := repository.ReadTenant(t.Context(), test.organization, corestore.Serving, func(reader tenantstore.Reader) error {
			used, err := reader.BillingUsage(t.Context(), now, "ingested_events")
			if errors.Is(err, corestore.ErrNotFound) {
				used, err = 0, nil
			}
			if err == nil && used != test.wantEvents {
				return fmt.Errorf("%s ingested events=%d want %d", test.organization, used, test.wantEvents)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
