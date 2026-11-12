package credits

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestAICreditUsageDebit(t *testing.T) {
	repository := hubtest.Repository(t)
	for index, test := range []struct {
		name             string
		input            int64
		multiplier       float64
		wantDebit        int64
		wantTransactions int
	}{
		{"ten cents with markup", 1000000, 1.5, 150000, 1},
		{"sub-cent exact micros", 10000, 1.5, 1500, 1},
		{"fractional micro rounds up", 1, 1.5, 1, 1},
		{"fractional micro above one", 10, 1.5, 2, 1},
		{"exact micro boundary", 20, 1.5, 3, 1},
		{"zero cost", 0, 1.5, 0, 0},
		{"custom markup", 1000000, 2, 200000, 1},
		{"at cost", 1000000, 1, 100000, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, suffix := range []string{"first", "second"} {
				t.Run(suffix, func(t *testing.T) {
					d := creditFixture(t, repository, 5000000)
					organization := d.deps.Organization
					projectID := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{ID: tracker.ProjectID(fmt.Sprintf("project_credit_usage_%d_%s", index, suffix))}).ID
					conversationID := conversation.NewConversationID()
					principal := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{projectID}})
					d.deps.CostMultiplier = test.multiplier
					at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
					d.deps.Now = func() time.Time { return at }
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: projectID, ID: conversationID, OwnerPrincipalID: principal.ID, Origin: "user", Title: "Credit usage", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: at}, Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at}); err != nil {
							return err
						}
						if err := w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: organization, ID: 1, Provider: "openai", Model: "gpt-6-luna", EffectiveAt: at.Add(-time.Hour), Input: .1, CachedInput: .01, Output: .5}); err != nil {
							return err
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					usage := Usage{OrganizationID: organization, ProjectID: projectID, ConversationID: conversationID, TurnID: "credit-usage-turn", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: test.input}, Outcome: conversation.DeliveryCompleted, OccurredAt: at}
					for range 2 {
						if err := d.RecordUsage(t.Context(), usage); err != nil {
							t.Fatal(err)
						}
					}
					view, err := d.Read(t.Context())
					if err != nil || view.BalanceMicros != 5000000-test.wantDebit || len(view.History) != test.wantTransactions || test.wantTransactions > 0 && (view.History[0].AmountMicros != -test.wantDebit || view.History[0].Kind != "usage") {
						t.Fatalf("credits=%+v err=%v", view, err)
					}
					window := Window{From: at, To: at.Add(time.Hour)}
					summary, err := d.UsageSummary(t.Context(), organization, window, nil)
					wantCost := float64(test.input) * .1 / 1000000
					if err != nil || summary.Turns != 1 || math.Abs(summary.CostUSD-wantCost) > 1e-12 {
						t.Fatalf("raw usage=%+v want cost=%g err=%v", summary, wantCost, err)
					}
					rows, err := d.UsageBuckets(t.Context(), organization, window, []string{string(projectID)})
					if err != nil || len(rows) != 1 || math.Abs(rows[0].CostUSD-wantCost) > 1e-12 {
						t.Fatalf("raw report rows=%+v err=%v", rows, err)
					}
					usage.TurnID = "own-provider-turn"
					usage.Provider = "codex"
					if err := d.RecordUsage(t.Context(), usage); err != nil {
						t.Fatal(err)
					}
					view, err = d.Read(t.Context())
					if err != nil || view.BalanceMicros != 5000000-test.wantDebit || len(view.History) != test.wantTransactions {
						t.Fatalf("own-provider credits=%+v err=%v", view, err)
					}
				})
			}
		})
	}
}
