package credits

import (
	"errors"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestPlatformAICreditAdjustments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, amount, reason string
		initial, wantBalance int64
		conflict, rollback   bool
		wantError            bool
	}{
		{name: "complimentary grant", amount: "10.25", reason: "design partner", wantBalance: 10250000},
		{name: "correction", amount: "-2.50", reason: "correction", initial: 10000000, wantBalance: 7500000},
		{name: "micro precision", amount: "0.000001", reason: "rounding correction", wantBalance: 1},
		{name: "conflicting replay", amount: "10", reason: "design partner", wantBalance: 10000000, conflict: true},
		{name: "zero", amount: "0", reason: "invalid", wantError: true},
		{name: "missing reason", amount: "10", reason: "  ", wantError: true},
		{name: "excess precision", amount: "0.0000001", reason: "invalid", wantError: true},
		{name: "overflow", amount: "1", reason: "invalid", initial: math.MaxInt64, wantBalance: math.MaxInt64, wantError: true},
		{name: "underflow", amount: "-1", reason: "invalid", initial: math.MinInt64, wantBalance: math.MinInt64, wantError: true},
		{name: "failed balance write rolls back ledger", amount: "10", reason: "design partner", rollback: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			admin := hubtest.Database(t)
			repository := hubtest.RepositoryOn(t, admin)
			d := creditFixture(t, repository, test.initial)
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			d.deps.Now = func() time.Time { return now }
			if test.rollback {
				if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION reject_credit_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'balance write failed'; END $$; CREATE TRIGGER reject_credit_update BEFORE UPDATE OF balance_micros ON ai_credit_accounts FOR EACH ROW EXECUTE FUNCTION reject_credit_update()`); err != nil {
					t.Fatal(err)
				}
			}
			adjustment := billing.CreditAdjustment{IdempotencyKey: "comp-credit-1", AmountUSD: test.amount, Reason: test.reason}
			for range 2 {
				balance, err := d.Adjust(t.Context(), "staff@example.test", adjustment)
				if (err != nil) != test.wantError || err == nil && balance != test.wantBalance {
					t.Fatalf("balance=%d err=%v", balance, err)
				}
			}
			if test.conflict {
				for _, changed := range []billing.CreditAdjustment{
					{IdempotencyKey: adjustment.IdempotencyKey, AmountUSD: "11", Reason: adjustment.Reason},
					{IdempotencyKey: adjustment.IdempotencyKey, AmountUSD: adjustment.AmountUSD, Reason: "different reason"},
				} {
					_, err := d.Adjust(t.Context(), "staff@example.test", changed)
					var conflict *hubapi.Error
					if !errors.As(err, &conflict) || conflict.Status != http.StatusConflict {
						t.Fatalf("conflicting replay=%v", err)
					}
				}
			}
			view, err := d.Read(t.Context())
			if err != nil || view.BalanceMicros != test.wantBalance {
				t.Fatalf("credits=%+v err=%v", view, err)
			}
			if test.wantError {
				if len(view.History) != 0 {
					t.Fatalf("rejected adjustment has ledger rows: %+v", view.History)
				}
				return
			}
			if len(view.History) != 1 {
				t.Fatalf("history=%+v", view.History)
			}
			item := view.History[0]
			if item.AmountMicros != test.wantBalance-test.initial || item.Kind != "complimentary" || item.Actor != "staff@example.test" || item.Reason != test.reason || !item.At.Equal(d.deps.Now()) {
				t.Fatalf("ledger=%+v", item)
			}
		})
	}
}
