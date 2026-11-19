package hubserver

import (
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
		service := &Service{database: conversationAllowanceDatabase(test.organization, ref, allowances, repository), config: Config{Clock: func() time.Time { return now }}}
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
