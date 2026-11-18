package credits

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIncludedAICreditUsage(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	organization := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
	principal := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
	d := fixtureService(t, repository, organization, hubtest.NewDetails())
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	plans := []billingstore.Plan{
		{PlanReference: billingstore.PlanReference{ID: "base", Version: 1}},
		{PlanReference: billingstore.PlanReference{ID: "starter", Version: 1}, Name: "Starter", MonthlyUSDCents: new(int64(4900))},
		{PlanReference: billingstore.PlanReference{ID: "team", Version: 2}, Name: "Team", MonthlyUSDCents: new(int64(9900))},
	}
	hubtest.PlanCatalog(t, repository, organization, plans)
	setSubscription := func(org tracker.OrganizationID, from, to, access time.Time, ref billingstore.PlanReference) {
		t.Helper()
		hubtest.PlanAssignment(t, repository, org, hubtest.PlanAssignmentOptions{Base: plans[0].PlanReference, Subscription: &ref, StartsAt: from, EndsAt: to, AccessUntil: access})
	}
	setSubscription(organization, start, end, end.Add(time.Hour), plans[1].PlanReference)
	projectID, conversationID := project.ID, "conv_0123456789abcdef0123456789abcdef"
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: billing.ModeTest, BalanceMicros: 5000000}); err != nil {
			return err
		}
		if err := w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: organization, ID: 1, Provider: "openai", Model: "gpt-6-luna", EffectiveAt: start.Add(-time.Hour), Input: .1}); err != nil {
			return err
		}
		_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: projectID, ID: conversationID, OwnerPrincipalID: principal.ID, Origin: "user", Title: "Included credit usage", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.DefaultPreferences(), CreatedAt: start, UpdatedAt: start})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name                    string
		at                      time.Time
		tokens                  int64
		included, balance, used int64
	}{
		{"within allowance", start, 32000000, 4800000, 5000000, 4800000},
		{"crosses allowance", start.Add(time.Hour), 1000000, 100000, 4950000, 4900000},
		{"exhausted", start.Add(2 * time.Hour), 1000000, 0, 4800000, 4900000},
		{"renewal", end, 1000000, 150000, 4800000, 150000},
		{"renewal discards unused allowance", end.AddDate(0, 1, 0), 1000000, 150000, 4800000, 150000},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !test.at.Before(end) {
				setSubscription(organization, test.at, test.at.AddDate(0, 1, 0), test.at.AddDate(0, 1, 0), billingstore.PlanReference{ID: "starter", Version: 1})
			}
			usage := Usage{OrganizationID: organization, ProjectID: projectID, ConversationID: conversationID, TurnID: test.name, Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: test.tokens}, Outcome: conversation.DeliveryCompleted, OccurredAt: test.at}
			for range 2 {
				if err := d.RecordUsage(t.Context(), usage); err != nil {
					t.Fatal(err)
				}
			}
			var balance, amount, included, used int64
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				account, err := r.BillingCreditAccount(t.Context(), billing.ModeTest)
				if err != nil {
					return err
				}
				row, err := r.BillingCreditTransactionBySource(t.Context(), billing.ModeTest, "usage:"+test.name)
				balance, amount, included = account.BalanceMicros, row.AmountMicros, row.IncludedAmountMicros
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if balance != test.balance || included != test.included || amount != -test.tokens*15/100 {
				t.Fatalf("balance=%d included=%d charged=%d", balance, included, amount)
			}
			periodStart := start
			if !test.at.Before(end) {
				periodStart = test.at
			}
			periodEnd := periodStart.AddDate(0, 1, 0)
			err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				period, err := r.BillingCreditPeriod(t.Context(), billing.ModeTest, periodStart, periodEnd)
				used = period.UsedMicros
				return err
			})
			if err != nil || used != test.used {
				t.Fatalf("period used=%d want=%d err=%v", used, test.used, err)
			}
		})
	}
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_second_credits"})
	if err := repository.WriteTenant(t.Context(), "org_second_credits", corestore.Serving, func(w tenantstore.Writer) error {
		for _, plan := range plans {
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: "org_second_credits", PlanReference: billingstore.PlanReference{ID: plan.ID, Version: plan.Version}, Name: plan.Name, MonthlyUSDCents: plan.MonthlyUSDCents, Allowances: plan.Allowances, Features: plan.Features}); err != nil {
				return err
			}
		}
		if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: "org_second_credits", Base: billingstore.PlanReference{ID: plans[0].ID, Version: plans[0].Version}}, 0); err != nil {
			return err
		}
		return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: "org_second_credits", Mode: billing.ModeTest})
	}); err != nil {
		t.Fatal(err)
	}
	setSubscription("org_second_credits", start, end, end, billingstore.PlanReference{ID: "team", Version: 2})
	var used int64
	err := repository.WriteTenant(t.Context(), "org_second_credits", corestore.Serving, func(w tenantstore.Writer) error {
		credits, err := d.IncludedFromReader(t.Context(), w, start)
		if err != nil {
			return err
		}
		if _, err := w.PostBillingUsageCredit(t.Context(), billingstore.CreditPeriod{OrganizationID: "org_second_credits", Mode: billing.ModeTest, StartsAt: start, EndsAt: end, AllowanceMicros: credits.AllowanceMicros}, billingstore.CreditTransaction{OrganizationID: "org_second_credits", Mode: billing.ModeTest, Source: "usage:second", AmountMicros: -9000000, Kind: "usage", RecordedAt: start}, 9000000); err != nil {
			return err
		}
		period, err := w.BillingCreditPeriod(t.Context(), billing.ModeTest, start, end)
		used = period.UsedMicros
		return err
	})
	if err != nil || used != 9000000 {
		t.Fatalf("second organization used=%d err=%v", used, err)
	}
	for _, test := range []struct {
		org  tracker.OrganizationID
		at   time.Time
		want int64
	}{{organization, end.AddDate(0, 1, 0), 4750000}, {"org_second_credits", start, 900000}} {
		credits, err := d.Included(t.Context(), test.org, test.at)
		if err != nil || credits.Remaining() != test.want {
			t.Fatalf("org=%s credits=%+v err=%v", test.org, credits, err)
		}
	}
}
