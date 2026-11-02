package hubserver

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestConversationUsageAndCreditsCommitTogetherInPostgres(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}})
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"First example", "Second example"} {
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(org.ID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "create", "name": "Example project", "states": nativeFixtureStates()})
		requireNativeStatus(t, response, http.StatusOK)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
		const conversationID = "conv_0123456789abcdef0123456789abcdef"
		err = repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: org.ID, ProjectID: project.ID, ID: conversationID, OwnerPrincipalID: "test_hub_admin", Origin: "user", Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at})
			if err != nil {
				return err
			}
			return w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: org.ID, ID: 1, Provider: "openai", Model: "example-model", EffectiveAt: at.Add(-time.Hour), Input: .1, CachedInput: .01, Output: .5})
		})
		if err != nil {
			t.Fatal(err)
		}
		d := &database{billingRepository: repository, aiCreditMode: "test", aiCreditCostMultiplier: 1.5}
		usage := ConversationUsage{OrganizationID: org.ID, ProjectID: project.ID, ConversationID: conversationID, TurnID: "turn-first", Provider: "openai", Model: "example-model", OccurredAt: at, Tokens: runner.AgentTokenCounts{InputTokens: 1000000}, Outcome: conversation.DeliveryCompleted}
		if err := d.RecordConversationUsage(t.Context(), usage); err == nil {
			t.Fatal("usage accepted without a credit account")
		}
		err = repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.ConversationUsage(t.Context(), usage.TurnID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("failed debit retained usage: %v", err)
			}
			return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: org.ID, Mode: "test", BalanceMicros: 5000000})
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
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
			summary, err := d.chatUsageSummary(t.Context(), org.ID, usageWindow{From: at, To: at.Add(time.Hour)}, selection.projects)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Turns != selection.turns || summary.Input != selection.turns*1000000 || summary.CostUSD != float64(selection.turns)*.1 {
				t.Fatalf("%s summary=%+v", selection.name, summary)
			}
		}
		err = repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
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
			if err := d.RecordConversationUsage(t.Context(), next); err != nil {
				t.Fatal(err)
			}
		}
		reporter := &Service{database: d, config: Config{Hosted: &HostedConfig{OrganizationID: string(org.ID)}}}
		for _, hourly := range []bool{false, true} {
			window := usageWindow{From: at, To: at.Add(2 * time.Hour), Hourly: hourly}
			rows, err := reporter.chatUsageRows(t.Context(), window, []string{string(project.ID)})
			if err != nil {
				t.Fatal(err)
			}
			expectedBuckets := 1
			if hourly {
				expectedBuckets = 2
			}
			if len(rows) != expectedBuckets {
				t.Fatalf("hourly=%v rows=%+v", hourly, rows)
			}
			report := buildUsageReport(window, rows, nil, nil, UsageConfig{})
			if report.Total.Sessions != 3 || report.Total.Tokens != 3000000 || report.Total.Cost != .3 || len(report.Providers) != 1 || report.Providers[0].Sessions != 3 || len(report.Daily) != expectedBuckets {
				t.Fatalf("hourly=%v report=%+v", hourly, report)
			}
		}
	}
}
