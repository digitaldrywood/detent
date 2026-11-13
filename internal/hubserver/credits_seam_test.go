package hubserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCreditUsageReport(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	service := openPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}})
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"First example", "Second example"} {
		org, err := service.createNativeOrganizationFor(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seedPostgresHubAdmin(t, repository, service, org)
		response := performHubAPIRequest(t, service, http.MethodPost, "/api/v2/organizations/"+string(org.ID)+"/projects", testHubAdminToken, map[string]any{"idempotency_key": "create", "name": "Example project", "grant_access": true, "states": nativeFixtureStates()})
		requireNativeStatus(t, response, http.StatusCreated)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
		const conversationID = "conv_0123456789abcdef0123456789abcdef"
		if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: org.ID, ProjectID: project.ID, ID: conversationID, OwnerPrincipalID: "test_hub_admin", Origin: "user", Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Execution: conversation.Execution{Status: conversation.ExecutionIdle}, Preferences: conversation.DefaultPreferences(), CreatedAt: at, UpdatedAt: at}); err != nil {
				return err
			}
			return w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: org.ID, ID: 1, Provider: "openai", Model: "example-model", EffectiveAt: at.Add(-time.Hour), Input: .1, CachedInput: .01, Output: .5})
		}); err != nil {
			t.Fatal(err)
		}
		d := &database{billingRepository: repository, billingAuditStore: &memoryRunDetails{}}
		for i, offset := range []time.Duration{0, time.Minute, time.Hour} {
			usage := ConversationUsage{OrganizationID: org.ID, ProjectID: project.ID, ConversationID: conversationID, TurnID: []string{"first", "second", "third"}[i], Provider: "openai", Model: "example-model", OccurredAt: at.Add(offset), Tokens: runner.AgentTokenCounts{InputTokens: 1000000}, Outcome: conversation.DeliveryCompleted}
			if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
				t.Fatal(err)
			}
		}
		reporter := &Service{database: d, config: Config{Hosted: &hostedplans.HostedConfig{OrganizationID: string(org.ID)}}}
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
