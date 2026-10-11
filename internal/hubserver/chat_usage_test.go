package hubserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/genkitbackend"
	"github.com/digitaldrywood/detent/internal/runner"
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		var input, cached, output, reasoning int64
		err := f.service.database.db.QueryRowContext(t.Context(), "SELECT input, cached_input, output, reasoning_output FROM conversation_usage WHERE turn_id=? AND organization_id=? AND conversation_id=?", assistant.ID, f.organization, record.ID).Scan(&input, &cached, &output, &reasoning)
		if err == nil {
			if input != 20 || cached != 5 || output != 10 || reasoning != 3 {
				t.Fatalf("tokens = %d %d %d %d", input, cached, output, reasoning)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable usage missing: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatUsagePriceVersionsAndPeriods(t *testing.T) {
	t.Parallel()
	f := newCoordinatorFixture(t, "usage-versions")
	record := f.seed(t, "Usage", nil)
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	next := start.AddDate(0, 1, 0)
	change := start.AddDate(0, 0, 15)
	_, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO conversation_prices(provider,model,effective_at,input,cached_input,output) VALUES('openai','gpt-6-luna',?,.20,.02,1)", change.UnixMicro())
	if err != nil {
		t.Fatal(err)
	}
	for i, at := range []time.Time{start.Add(-time.Microsecond), start, change.Add(-time.Microsecond), change, next} {
		usage := ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: fmt.Sprintf("turn-%d", i), Provider: "openai", Model: "gpt-6-luna", OccurredAt: at, Tokens: runner.AgentTokenCounts{InputTokens: 1_000_000, CachedInputTokens: 400_000, OutputTokens: 100_000, ReasoningOutputTokens: 30_000}, Outcome: conversation.DeliveryCompleted}
		for range 2 {
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
	if _, err := recordTestCost(t, f.service.database.db, string(f.organization), string(f.project.ID), infrastructure, next); err != nil {
		t.Fatal(err)
	}
	paid, quantity := int64(70_000), 100.0
	runnerCost := costObservation{Provider: "codex", ProviderAccount: "runner", ResourceID: "attempt", AttemptID: "attempt", WorkItemID: "issue", Model: "gpt-6-sol", BillingMode: "metered", UsageKind: "cumulative", Bucket: runnerCostBucket, Metric: "gpt-6-sol", SourceID: "runner-source", Revision: 1, From: start, To: next, Quantity: &quantity, Unit: "token", QuantityBasis: "provider_reported", AmountMicros: &paid, ReportedAmountMicros: &paid, Currency: "USD", Basis: "runner_reported", EvidenceSource: "runner_report", ObservedAt: next, FreshUntil: next, Coverage: "complete"}
	if _, err := recordTestCost(t, f.service.database.db, string(f.organization), string(f.project.ID), runnerCost, next); err != nil {
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
		t.Fatalf("distinct charges combined incorrectly: %+v", report.Totals)
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
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

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
	deadline := time.Now().Add(5 * time.Second)
	for {
		var cost float64
		var provider string
		err := f.service.database.db.QueryRowContext(t.Context(), "SELECT provider,cost_usd FROM conversation_usage WHERE turn_id=?", assistant.ID).Scan(&provider, &cost)
		if err == nil {
			if provider != "openai" || math.Abs(cost-.114) > 1e-12 {
				t.Fatalf("provider=%s cost=%v", provider, cost)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage not persisted: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	databasePath := f.service.config.DatabasePath
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestService(t, Config{DatabasePath: filepath.Clean(databasePath)})
	var cost float64
	if err := reopened.database.db.QueryRowContext(t.Context(), "SELECT cost_usd FROM conversation_usage WHERE turn_id=?", assistant.ID).Scan(&cost); err != nil || math.Abs(cost-.114) > 1e-12 {
		t.Fatalf("reopened cost=%v error=%v", cost, err)
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
			case <-time.After(5 * time.Second):
				t.Fatal("usage update missing")
			}
			if test.interrupt {
				if !f.coordinator().Cancel(record.ID) {
					t.Fatal("turn was not running")
				}
			}
			assistant := f.waitAssistant(t, record.ID, test.outcome)
			deadline := time.Now().Add(5 * time.Second)
			for {
				var outcome string
				var input, output int64
				err := f.service.database.db.QueryRowContext(t.Context(), "SELECT outcome,input,output FROM conversation_usage WHERE turn_id=?", assistant.ID).Scan(&outcome, &input, &output)
				if err == nil {
					if outcome != string(test.outcome) || input != 10 || output != 5 {
						t.Fatalf("usage = %s %d %d", outcome, input, output)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("usage missing: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
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
		waitUntil(t, "metered turn", func() bool {
			var count int
			err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_usage WHERE conversation_id=? AND model='example-resolved'", record.ID).Scan(&count)
			return err == nil && count == turn
		})
	}
	if f.backend.request(t, 1).Resume.ThreadID != "resumed-thread" {
		t.Fatal("thread was not resumed")
	}
	var input, cached, output, reasoning int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT sum(input),sum(cached_input),sum(output),sum(reasoning_output) FROM conversation_usage WHERE conversation_id=?", record.ID).Scan(&input, &cached, &output, &reasoning); err != nil {
		t.Fatal(err)
	}
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
		name, trigger string
		want          conversation.Delivery
		wantRows      int
	}{
		{"completion sees usage", `CREATE TRIGGER completion_requires_usage BEFORE UPDATE OF delivery ON conversation_messages WHEN NEW.role='assistant' AND NEW.delivery='completed' AND NOT EXISTS(SELECT 1 FROM conversation_usage WHERE turn_id=NEW.id) BEGIN SELECT RAISE(ABORT,'completion must include usage'); END`, conversation.DeliveryCompleted, 1},
		{"usage failure rolls back completion", `CREATE TRIGGER fail_usage BEFORE INSERT ON conversation_usage BEGIN SELECT RAISE(ABORT,'example usage write failure'); END`, conversation.DeliveryResponding, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCoordinatorFixture(t, "atomic-usage")
			f.conversations.config.Model = "example-model"
			if _, err := f.service.database.db.ExecContext(t.Context(), test.trigger); err != nil {
				t.Fatal(err)
			}
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
			var rows int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_usage WHERE turn_id=?", assistant.ID).Scan(&rows); err != nil || rows != test.wantRows {
				t.Fatalf("usage rows = %d, %v", rows, err)
			}
		})
	}
}
