package credits

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestConversationUsageAndCreditsCommitTogetherInPostgres(t *testing.T) {
	repository := hubtest.Repository(t)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"First example", "Second example"} {
		org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{Name: name})
		project := hubtest.Project(t, repository, org.OrganizationID, hubtest.ProjectOptions{})
		principal := hubtest.Credential(t, repository, org.OrganizationID, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}})
		const conversationID = "conv_0123456789abcdef0123456789abcdef"
		err := repository.WriteTenant(t.Context(), org.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: org.OrganizationID, ProjectID: project.ID, ID: conversationID, OwnerPrincipalID: principal.ID, Origin: "user", Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at})
			if err != nil {
				return err
			}
			return w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: org.OrganizationID, ID: 1, Provider: "openai", Model: "example-model", EffectiveAt: at.Add(-time.Hour), Input: .1, CachedInput: .01, Output: .5})
		})
		if err != nil {
			t.Fatal(err)
		}
		details := hubtest.NewDetails()
		d := fixtureService(t, repository, org.OrganizationID, details)
		usage := Usage{OrganizationID: org.OrganizationID, ProjectID: project.ID, ConversationID: conversationID, TurnID: "turn-first", Provider: "openai", Model: "example-model", OccurredAt: at, Tokens: runner.AgentTokenCounts{InputTokens: 1000000}, Outcome: conversation.DeliveryCompleted}
		if err := d.RecordUsage(t.Context(), usage); err == nil {
			t.Fatal("usage accepted without a credit account")
		}
		err = repository.WriteTenant(t.Context(), org.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.ConversationUsage(t.Context(), usage.TurnID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed debit retained usage: %v", err)
			}
			for _, scope := range []tracker.ProjectID{"", project.ID} {
				spend, err := w.BillingMonthlySpend(t.Context(), scope, at)
				if err != nil {
					return err
				}
				if spend.LunaMicros != 0 {
					t.Fatalf("failed debit retained monthly spend: %+v", spend)
				}
			}
			return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org.OrganizationID, Mode: "test", BalanceMicros: 5000000})
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := d.RecordUsage(t.Context(), usage); err != nil {
				t.Fatal(err)
			}
		}
		for _, selection := range []struct {
			name     string
			projects []string
			turns    int64
		}{
			{name: "all", turns: 1},
			{name: "selected", projects: []string{string(project.ID)}, turns: 1},
			{name: "empty", projects: []string{}, turns: 0},
			{name: "other", projects: []string{"other-project"}, turns: 0},
		} {
			summary, err := d.UsageSummary(t.Context(), org.OrganizationID, Window{From: at, To: at.Add(time.Hour)}, selection.projects)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Turns != selection.turns || summary.Input != selection.turns*1000000 || summary.CostUSD != float64(selection.turns)*.1 {
				t.Fatalf("%s summary=%+v", selection.name, summary)
			}
		}
		err = repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			account, err := r.BillingCreditAccount(t.Context(), "test")
			if err != nil {
				return err
			}
			if account.BalanceMicros != 4850000 {
				t.Fatalf("balance=%d", account.BalanceMicros)
			}
			recorded, err := r.ConversationUsage(t.Context(), usage.TurnID)
			if err != nil {
				return err
			}
			if recorded.CostUSD == nil || *recorded.CostUSD != .1 || recorded.ProjectID != project.ID {
				t.Fatalf("usage=%+v", recorded)
			}
			for _, scope := range []tracker.ProjectID{"", project.ID} {
				spend, err := r.BillingMonthlySpend(t.Context(), scope, at)
				if err != nil {
					return err
				}
				if spend.LunaMicros != 100000 {
					t.Fatalf("monthly spend after replay=%+v", spend)
				}
			}
			revision, err := r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: "openai", ProviderAccount: "luna", SourceID: "luna:" + usage.TurnID, From: at, To: at.Add(time.Microsecond)})
			if err != nil {
				return err
			}
			if revision.Reference.Key == "" || revision.ProjectID != project.ID {
				t.Fatalf("missing cost evidence: %+v", revision)
			}
			entries, err := r.BillingCreditTransactions(t.Context(), "test", 0, 10)
			if err == nil && (len(entries) != 1 || entries[0].AmountMicros != -150000) {
				t.Fatalf("credit history=%+v", entries)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		for i, offset := range []time.Duration{time.Minute, time.Hour} {
			next := usage
			next.TurnID = []string{"turn-second", "turn-third"}[i]
			next.OccurredAt = at.Add(offset)
			if err := d.RecordUsage(t.Context(), next); err != nil {
				t.Fatal(err)
			}
		}
		for _, failure := range []string{"store", "read"} {
			unavailable := &failingDetails{Details: hubtest.NewDetails(), fail: failure == "store"}
			if failure == "read" {
				unavailable.beforeRead = func(context.Context) error { return errors.New("archive unavailable") }
			}
			failed := usage
			failed.TurnID = "archive-failure-" + failure
			originalStore := d.deps.Details
			d.deps.Details = unavailable
			err := d.RecordUsage(t.Context(), failed)
			d.deps.Details = originalStore
			if err == nil {
				t.Fatalf("%s archive failure accepted", failure)
			}
			if err := repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				if _, err := r.ConversationUsage(t.Context(), failed.TurnID); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("archive failure retained usage: %v", err)
				}
				spend, err := r.BillingMonthlySpend(t.Context(), project.ID, at)
				if err == nil && spend.LunaMicros != 300000 {
					t.Fatalf("archive failure changed spend: %+v", spend)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		next := usage
		next.TurnID = "turn-repriced"
		next.OccurredAt = at.Add(2 * time.Hour)
		prepared, err := d.PrepareUsage(t.Context(), next)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: org.OrganizationID, ID: 2, Provider: "openai", Model: "example-model", EffectiveAt: at.Add(90 * time.Minute), Input: .2, CachedInput: .02, Output: 1})
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), org.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { return d.WriteUsage(t.Context(), w, prepared) }); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("price changed after preparation: %v", err)
		}
		if err := repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.ConversationUsage(t.Context(), next.TurnID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("stale preparation retained usage: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.RecordUsage(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			spend, err := r.BillingMonthlySpend(t.Context(), project.ID, at)
			if err == nil && spend.LunaMicros != 500000 {
				t.Fatalf("reprepared price spend: %+v", spend)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		historical := usage
		historical.TurnID = "historical-no-cost"
		if err := repository.WriteTenant(t.Context(), org.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.ConversationUsage(t.Context(), usage.TurnID)
			if err != nil {
				return err
			}
			row.TurnID = historical.TurnID
			_, err = w.RecordConversationUsage(t.Context(), row)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var balanceBefore int64
		if err := repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: "openai", ProviderAccount: "luna", SourceID: "luna:" + historical.TurnID, From: at, To: at.Add(time.Microsecond)}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("historical fixture already has cost: %v", err)
			}
			account, err := r.BillingCreditAccount(t.Context(), "test")
			balanceBefore = account.BalanceMicros
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := d.RecordUsage(t.Context(), historical); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			account, err := r.BillingCreditAccount(t.Context(), "test")
			if err != nil {
				return err
			}
			if account.BalanceMicros != balanceBefore {
				t.Fatalf("historical replay charged credit: %d -> %d", balanceBefore, account.BalanceMicros)
			}
			spend, err := r.BillingMonthlySpend(t.Context(), project.ID, at)
			if err == nil && spend.LunaMicros != 600000 {
				t.Fatalf("historical cost repair spend: %+v", spend)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

	}
}
