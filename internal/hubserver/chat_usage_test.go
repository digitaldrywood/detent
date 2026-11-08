package hubserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChatUsageCost(t *testing.T) {
	t.Parallel()
	price := UsagePrice{Input: .10, CachedInput: .01, Output: .50, Currency: "USD"}
	for _, test := range []struct {
		name   string
		tokens runner.AgentTokenCounts
		want   float64
	}{
		{"uncached", runner.AgentTokenCounts{InputTokens: 1_000_000}, .10},
		{"cached", runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 1_000_000}, .01},
		{"mixed with reasoning included in output", runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}, .114},
		{"tiny turn", runner.AgentTokenCounts{InputTokens: 10, OutputTokens: 10}, .000006},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := chatUsageCost(price, test.tokens)
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("cost = %.12f, want %.12f", got, test.want)
			}
		})
	}
}

func TestConversationCoordinatorPersistsUsage(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "persist-usage")
	f.conversations.config.Model = "gpt-6-luna"
	f.backend.setRun(func(_ context.Context, _ int, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 20, CachedInputTokens: 5, OutputTokens: 10, ReasoningOutputTokens: 3, TotalTokens: 30}})
		return runner.AgentTurnResult{}, err
	})
	record := f.seed(t, "Usage", nil)
	f.say(t, &record, "Hello")
	assistant := f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	testenv.WaitUntil(t, t.Context(), func() error {
		usage, err := readCoordinatorUsage(t, f.service, f.organization, assistant.ID)
		input, cached, output, reasoning := usage.Input, usage.CachedInput, usage.Output, usage.ReasoningOutput
		if err == nil && usage.ConversationID != record.ID {
			t.Fatalf("usage conversation=%s", usage.ConversationID)
		}
		if err != nil {
			return fmt.Errorf("durable usage missing: %w", err)
		}
		if input != 20 || cached != 5 || output != 10 || reasoning != 3 {
			t.Fatalf("tokens = %d %d %d %d", input, cached, output, reasoning)
		}
		return nil
	})
}

func TestChatUsagePriceVersionsAndPeriods(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "usage-versions")
	record := f.seed(t, "Usage", nil)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	next := start.AddDate(0, 1, 0)
	change := start.AddDate(0, 0, 15)
	f.write(t, func(w tenantstore.Writer) error {
		return w.CreateConversationPrice(t.Context(), billingstore.ConversationPrice{OrganizationID: f.organization, ID: 2, Provider: "openai", Model: "gpt-6-luna", EffectiveAt: change, Input: .2, CachedInput: .02, Output: 1})
	})
	for i, at := range []time.Time{start.Add(-time.Microsecond), start, change.Add(-time.Microsecond), change, next} {
		usage := ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: fmt.Sprintf("turn-%d", i), Provider: "openai", Model: "gpt-6-luna", OccurredAt: at, Tokens: runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}, Outcome: conversation.DeliveryCompleted}
		for range 2 {
			if i == 1 {
				f.write(t, func(w tenantstore.Writer) error {
					_, err := w.RecordConversationUsage(t.Context(), billingstore.ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: usage.TurnID, Provider: usage.Provider, Model: usage.Model, OccurredAt: usage.OccurredAt, Input: 1000000, CachedInput: 400000, Output: 100000, ReasoningOutput: 30000, Outcome: string(usage.Outcome), PriceID: new(int64(1)), CostUSD: new(.114), CacheSavingsUSD: new(.036)})
					return err
				})
				continue
			}
			if err := f.service.database.RecordConversationUsage(t.Context(), usage); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, test := range []struct {
		name     string
		from, to time.Time
		turns    int64
		cost     float64
		org      tracker.OrganizationID
		projects []string
	}{
		{"month", start, next, 3, .456, f.organization, nil},
		{"before price change", start, change, 2, .228, f.organization, nil},
		{"from price change", change, next, 1, .228, f.organization, nil},
		{"other organization", start, next, 0, 0, "other", nil},
		{"no grants", start, next, 0, 0, f.organization, []string{}},
		{"wrong project", start, next, 0, 0, f.organization, []string{"other"}},
		{"project grant", start, next, 3, .456, f.organization, []string{string(f.project.ID)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := f.service.database.chatUsageSummary(t.Context(), test.org, usageWindow{From: test.from, To: test.to}, test.projects)
			if err != nil || got.Turns != test.turns || math.Abs(got.CostUSD-test.cost) > 1e-12 || got.Tokens != test.turns*1_100_000 || got.ReasoningOutput != test.turns*30_000 {
				t.Fatalf("summary = %+v, %v", got, err)
			}
		})
	}

	period := usageWindow{From: start, To: next}
	infrastructure := costTestObservation("cpu", start, next)
	if _, err := recordPostgresTestCost(t, f.service.database, string(f.organization), string(f.project.ID), infrastructure, next); err != nil {
		t.Fatal(err)
	}
	paid, quantity := int64(70_000), 100.0
	runnerCost := costObservation{Provider: "codex", ProviderAccount: "runner", ResourceID: "attempt", AttemptID: "attempt", WorkItemID: "issue", Model: "gpt-6-sol", BillingMode: "metered", UsageKind: "cumulative", Bucket: runnerCostBucket, Metric: "gpt-6-sol", SourceID: "runner-source", Revision: 1, From: start, To: next, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", AmountMicros: &paid, ReportedAmountMicros: &paid, Currency: "USD", Basis: "runner_reported", EvidenceSource: "runner_report", ObservedAt: next, FreshUntil: next, Coverage: "complete"}
	if _, err := recordPostgresTestCost(t, f.service.database, string(f.organization), string(f.project.ID), runnerCost, next); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.database.monthlyCosts(t.Context(), string(f.organization), "organization", nil, period, next)
	if err != nil {
		t.Fatal(err)
	}
	amounts := map[string]int64{}
	for _, total := range report.Totals {
		if total.KnownMicros != nil {
			amounts[total.Bucket] = *total.KnownMicros
		}
	}
	if amounts[lunaCostBucket] != 456_000 || amounts[runnerCostBucket] != 70_000 || amounts[spriteCostBucket] != 20_000 {
		t.Fatalf("distinct charges combined incorrectly: amounts=%v totals=%+v", amounts, report.Totals)
	}
	if len(report.ByProject) != 1 || len(report.ByProject[0].Totals) != 3 {
		t.Fatalf("project breakdown: %+v", report.ByProject)
	}
}

func TestChatBillingWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		subscription billing.Snapshot
		from, to     time.Time
	}{
		{"free", billing.Snapshot{}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"paid", billing.Snapshot{PeriodStart: now.AddDate(0, 0, -15), PeriodEnd: now.AddDate(0, 0, 15)}, now.AddDate(0, 0, -15), now.AddDate(0, 0, 15)},
		{"expired", billing.Snapshot{PeriodStart: now.AddDate(0, 0, -30), PeriodEnd: now}, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := chatBillingWindow(now, test.subscription)
			if !got.From.Equal(test.from) || !got.To.Equal(test.to) {
				t.Fatalf("window = %+v", got)
			}
		})
	}
}

func TestChatUsageUnknownPriceAndInvalidAttribution(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "unpriced")
	record := f.seed(t, "Usage", nil)
	now := time.Now().UTC()
	usage := ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: "unknown", Provider: "example", Model: "example-model", Tokens: runner.AgentTokenCounts{InputTokens: 10, OutputTokens: 5}}
	if err := f.service.database.RecordConversationUsage(t.Context(), usage); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.database.chatUsageSummary(t.Context(), f.organization, usageWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, nil)
	if err != nil || got.UnpricedTurns != 1 || got.Tokens != 15 || got.CostUSD != 0 {
		t.Fatalf("summary = %+v, %v", got, err)
	}

	month, _ := costMonth(now.Format("2006-01"), now)
	report, err := f.service.database.monthlyCosts(t.Context(), string(f.organization), "organization", nil, month, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Totals) != 1 || report.Totals[0].Bucket != lunaCostBucket || report.Totals[0].KnownMicros != nil || report.Totals[0].Unknown != 1 {
		t.Fatalf("unpriced Luna usage became free: %+v", report)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ConversationUsage)
	}{
		{"organization", func(u *ConversationUsage) { u.OrganizationID = "other" }},
		{"project", func(u *ConversationUsage) { u.ProjectID = "other" }},
		{"conversation", func(u *ConversationUsage) { u.ConversationID = "other" }},
		{"negative", func(u *ConversationUsage) { u.Tokens.InputTokens = -1 }},
		{"cached exceeds input", func(u *ConversationUsage) { u.Tokens.CachedInputTokens = 11 }},
		{"reasoning exceeds output", func(u *ConversationUsage) { u.Tokens.ReasoningOutputTokens = 6 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := usage
			test.mutate(&bad)
			if err := f.service.database.RecordConversationUsage(t.Context(), bad); err == nil {
				t.Fatal("invalid usage accepted")
			}
		})
	}
}

func TestHostedChatUsageReports(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", browserPreviewConfig)
	f.seedPreview(t)
	usage := ConversationUsage{OrganizationID: "org_browser_preview", ProjectID: tracker.ProjectID(f.project), ConversationID: f.conversation, TurnID: "report-chat", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}}
	if err := f.service.database.RecordConversationUsage(t.Context(), usage); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{"owner", "viewer"} {
		t.Run(account, func(t *testing.T) {
			var report usageReport
			response := f.page(t, account, browserHostedOrganizationBase+"/usage?range=30d")
			browserHostedStatus(t, response, http.StatusOK)
			browserHostedDecode(t, response, &report)
			if report.Chat.Tokens != 1_100_000 || math.Abs(report.Chat.CostUSD-.114) > 1e-12 || report.Total.Tokens != 1_100_000 || report.Total.Sessions != 1 || len(report.Runners) != 0 || math.Abs(report.Total.Cost-.114) > 1e-12 || math.Abs(report.Totals.CacheSavings-.036) > 1e-12 {
				t.Fatalf("report = %+v", report)
			}
		})
	}
	var filtered usageReport
	response := f.page(t, "owner", browserHostedOrganizationBase+"/usage?project="+f.privateProject)
	browserHostedStatus(t, response, http.StatusOK)
	browserHostedDecode(t, response, &filtered)
	if filtered.Chat.Turns != 0 || filtered.Total.Tokens != 0 {
		t.Fatalf("project leaked usage: %+v", filtered)
	}
	var billing hostedBillingView
	response = f.page(t, "owner", browserHostedOrganizationBase+"/billing")
	browserHostedStatus(t, response, http.StatusOK)
	browserHostedDecode(t, response, &billing)
	if billing.ChatUsage.Tokens != 1_100_000 || math.Abs(billing.ChatUsage.CostUSD-.114) > 1e-12 {
		t.Fatalf("billing = %+v", billing.ChatUsage)
	}
}

type chatUsageTransport func(*http.Request) (*http.Response, error)

func (f chatUsageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGenkitCoordinatorPersistsPricedUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Done."}]}],"usage":{"input_tokens":1000000,"output_tokens":100000,"total_tokens":1100000,"input_tokens_details":{"cached_tokens":400000},"output_tokens_details":{"reasoning_tokens":30000}}}}

`)
	}))
	defer server.Close()
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: chatUsageTransport(func(request *http.Request) (*http.Response, error) {
		clone := request.Clone(request.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return server.Client().Transport.RoundTrip(clone)
	})}
	backend, err := genkitbackend.NewOpenAI("example-key")
	http.DefaultClient = originalClient
	if err != nil {
		t.Fatal(err)
	}
	f := newCoordinatorFixture(t, "genkit-usage")
	f.conversations.config.Backend = backend
	f.conversations.config.Model = genkitbackend.Model
	record := f.seed(t, "Usage", nil)
	f.say(t, &record, "Hello")
	assistant := f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
	testenv.WaitUntil(t, t.Context(), func() error {
		usage, err := readCoordinatorUsage(t, f.service, f.organization, assistant.ID)
		if err != nil {
			return fmt.Errorf("usage not persisted: %w", err)
		}
		if usage.Provider != "openai" || usage.CostUSD == nil || math.Abs(*usage.CostUSD-.114) > 1e-12 {
			t.Fatalf("persisted usage=%+v", usage)
		}
		return nil
	})
	admin := usagePostgresAdmin(t, f.service)
	reopenConfig := f.service.config
	defaults := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	reopenConfig.Postgres = defaults.Postgres
	reopenConfig.Hosted = defaults.Hosted
	reopenConfig.Hosted.OrganizationID = string(f.organization)
	reopenConfig.TenantRepository, reopenConfig.AccessRepository, reopenConfig.BillingRepository = nil, nil, nil
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestService(t, reopenConfig)
	usage, err := readCoordinatorUsage(t, reopened, f.organization, assistant.ID)
	if err != nil || usage.CostUSD == nil || math.Abs(*usage.CostUSD-.114) > 1e-12 {
		t.Fatalf("reopened usage=%+v error=%v", usage, err)
	}
}

func TestConversationCoordinatorMetersFailedAndInterruptedTurns(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		interrupt bool
		outcome   conversation.Delivery
	}{
		{"failed", false, conversation.DeliveryFailed},
		{"interrupted", true, conversation.DeliveryInterrupted},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, "usage-"+test.name)
			f.conversations.config.Model = genkitbackend.Model
			record := f.seed(t, "Usage", nil)
			reported := make(chan struct{})
			f.backend.setRun(func(ctx context.Context, _ int, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
				if err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 10, OutputTokens: 5}}); err != nil {
					return runner.AgentTurnResult{}, err
				}
				close(reported)
				if test.interrupt {
					<-ctx.Done()
					return runner.AgentTurnResult{}, ctx.Err()
				}
				return runner.AgentTurnResult{}, errors.New("example provider failure")
			})
			f.say(t, &record, "Hello")
			select {
			case <-reported:
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("usage update missing")
			}
			if test.interrupt {
				if !f.coordinator().Cancel(record.ID) {
					t.Fatal("turn was not running")
				}
			}
			assistant := f.waitAssistant(t, record.ID, test.outcome)
			testenv.WaitUntil(t, t.Context(), func() error {
				usage, err := readCoordinatorUsage(t, f.service, f.organization, assistant.ID)
				outcome, input, output := usage.Outcome, usage.Input, usage.Output
				if err != nil {
					return fmt.Errorf("usage missing: %w", err)
				}
				if outcome != string(test.outcome) || input != 10 || output != 5 {
					t.Fatalf("usage = %s %d %d", outcome, input, output)
				}
				return nil
			})
		})
	}
}

func TestConversationCoordinatorMetersResumedUsageAndResolvedModel(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "resumed-usage")
	f.backend.setRun(func(_ context.Context, turn int, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
		tokens := runner.AgentTokenCounts{InputTokens: 80, CachedInputTokens: 40, OutputTokens: 20, ReasoningOutputTokens: 5, TotalTokens: 100}
		last := tokens
		if turn == 2 {
			tokens = runner.AgentTokenCounts{InputTokens: 120, CachedInputTokens: 60, OutputTokens: 30, ReasoningOutputTokens: 7, TotalTokens: 150}
			last = runner.AgentTokenCounts{InputTokens: 40, CachedInputTokens: 20, OutputTokens: 10, ReasoningOutputTokens: 2, TotalTokens: 50}
		}
		err := update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, ThreadID: "resumed-thread", Model: "example-resolved", Tokens: runner.AgentTokenUsage{InputTokens: tokens.InputTokens, CachedInputTokens: tokens.CachedInputTokens, OutputTokens: tokens.OutputTokens, ReasoningOutputTokens: tokens.ReasoningOutputTokens, TotalTokens: tokens.TotalTokens, ThreadTotal: &tokens, Last: &last}})
		return runner.AgentTurnResult{ThreadID: "resumed-thread"}, err
	})
	record := f.seed(t, "Usage", nil)
	for turn := 1; turn <= 2; turn++ {
		f.say(t, &record, "Hello")
		f.waitAssistant(t, record.ID, conversation.DeliveryCompleted)
		waitUntil(t, "metered turn", func() bool {
			summary, err := f.service.database.chatUsageSummary(t.Context(), f.organization, usageWindow{From: time.Unix(0, 0).UTC(), To: time.Now().Add(time.Hour)}, nil)
			return err == nil && summary.Turns == int64(turn)
		})
	}
	if f.backend.request(t, 1).Resume.ThreadID != "resumed-thread" {
		t.Fatal("thread was not resumed")
	}
	var totals billingstore.ConversationUsageSummary
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.ConversationUsageBuckets(t.Context(), time.Unix(0, 0).UTC(), time.Now().Add(time.Hour), nil, false)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Model != "example-resolved" {
				t.Fatalf("usage model=%q", row.Model)
			}
		}
		totals, err = r.ConversationUsageSummary(t.Context(), time.Unix(0, 0).UTC(), time.Now().Add(time.Hour), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	input, cached, output, reasoning := totals.Input, totals.CachedInput, totals.Output, totals.ReasoningOutput
	if input != 120 || cached != 60 || output != 30 || reasoning != 7 {
		t.Fatalf("totals = %d %d %d %d", input, cached, output, reasoning)
	}
}

func TestChatCacheSavingsCurrency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		currency string
		want     float64
	}{{"USD", .036}, {"EUR", 0}} {
		t.Run(test.currency, func(t *testing.T) {
			report := buildUsageReport(usageWindow{}, []usageRow{{AttemptID: "chat:turn", Chat: true, Currency: "USD", Input: 1_000_000, CachedInput: 400_000, Output: 100_000, Cost: .114, CacheSavings: .036}}, nil, nil, UsageConfig{Currency: test.currency})
			if math.Abs(report.Totals.CacheSavings-test.want) > 1e-12 {
				t.Fatalf("savings = %v", report.Totals.CacheSavings)
			}
		})
	}
}

func TestConversationUsageCommitsWithCompletion(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		fail     bool
		want     conversation.Delivery
		wantRows int
	}{
		{"completion sees usage", false, conversation.DeliveryCompleted, 1},
		{"usage failure rolls back completion", true, conversation.DeliveryResponding, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, "atomic-usage")
			f.conversations.config.Model = "example-model"
			f.service.config.TenantRepository = coordinatorUsageRepository{Repository: f.service.config.TenantRepository, fail: test.fail}
			f.backend.setRun(func(_ context.Context, _ int, _ runner.AgentToolHandler, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
				return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateTokenUsage, Tokens: runner.AgentTokenUsage{InputTokens: 10, OutputTokens: 5}})
			})
			record := f.seed(t, "Usage", nil)
			f.say(t, &record, "Hello")
			f.backend.waitStarted(t)
			coordinator := f.coordinator().(*conversationTurnCoordinator)
			waitUntil(t, "turn finished", func() bool { coordinator.mu.Lock(); defer coordinator.mu.Unlock(); return len(coordinator.turns) == 0 })
			assistant, ok := lastReply(f.messages(t, record.ID))
			if !ok || assistant.Delivery != test.want {
				t.Fatalf("assistant = %+v, want %s", assistant, test.want)
			}
			_, err := readCoordinatorUsage(t, f.service, f.organization, assistant.ID)
			rows := 1
			if errors.Is(err, corestore.ErrNotFound) {
				rows = 0
				err = nil
			}
			if err != nil || rows != test.wantRows {
				t.Fatalf("usage rows = %d, %v", rows, err)
			}
		})
	}
}

func readCoordinatorUsage(t *testing.T, service *Service, organization tracker.OrganizationID, turn string) (billingstore.ConversationUsage, error) {
	t.Helper()
	var row billingstore.ConversationUsage
	err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.ConversationUsage(t.Context(), turn)
		return err
	})
	return row, err
}

type coordinatorUsageRepository struct {
	tenantstore.Repository
	fail bool
}

func (r coordinatorUsageRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, work corestore.Workload, fn func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, work, func(w tenantstore.Writer) error { return fn(coordinatorUsageWriter{Writer: w, fail: r.fail}) })
}

type coordinatorUsageWriter struct {
	tenantstore.Writer
	fail bool
}

func (w coordinatorUsageWriter) RecordConversationUsage(ctx context.Context, row billingstore.ConversationUsage) (bool, error) {
	if w.fail {
		return false, errors.New("example usage write failure")
	}
	return w.Writer.RecordConversationUsage(ctx, row)
}
func (w coordinatorUsageWriter) UpdateMessage(ctx context.Context, row conversationstore.Message, rev int64) (conversationstore.Message, error) {
	if row.Role == conversation.RoleAssistant && row.Delivery == conversation.DeliveryCompleted {
		if _, err := w.ConversationUsage(ctx, row.ID); err != nil {
			return conversationstore.Message{}, errors.New("completion must include usage")
		}
	}
	return w.Writer.UpdateMessage(ctx, row, rev)
}
