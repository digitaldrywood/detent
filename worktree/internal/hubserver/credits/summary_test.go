package credits

import (
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestChatUsageCost(t *testing.T) {
	t.Parallel()
	price := billingstore.ConversationPrice{Input: .10, CachedInput: .01, Output: .50}
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
			got := UsageCost(price, test.tokens)
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("cost = %.12f, want %.12f", got, test.want)
			}
		})
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
			got := BillingWindow(now, test.subscription)
			if !got.From.Equal(test.from) || !got.To.Equal(test.to) {
				t.Fatalf("window = %+v", got)
			}
		})
	}
}
