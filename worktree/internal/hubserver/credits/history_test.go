package credits

import (
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestAICreditGroupedHistory(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			repository := hubtest.Repository(t)
			d := creditFixture(t, repository, 35*2*(123-100)-3*(5000000+5000001))
			if err := repository.WriteTenant(t.Context(), d.deps.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: d.deps.Organization, Mode: billing.ModeLive, BalanceMicros: -999})
			}); err != nil {
				t.Fatal(err)
			}
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for day := range 35 {
				for turn := range 2 {
					hubtest.CreditGrant(t, repository, d.deps.Organization, billingstore.CreditTransaction{Source: fmt.Sprintf("usage:day-%d-turn-%d", day, turn), AmountMicros: -123, IncludedAmountMicros: 100, Kind: "usage", RecordedAt: start.AddDate(0, 0, day).Add(time.Duration(turn) * time.Hour)})
				}
			}
			for _, kind := range []string{"purchase", "auto_fund", "complimentary"} {
				for n := range 2 {
					hubtest.CreditGrant(t, repository, d.deps.Organization, billingstore.CreditTransaction{Source: fmt.Sprintf("%s-%d", kind, n), AmountMicros: int64(5000000 + n), Kind: kind, RecordedAt: start.AddDate(0, 0, 34).Add(2 * time.Hour)})
				}
			}
			hubtest.CreditGrant(t, repository, d.deps.Organization, billingstore.CreditTransaction{Mode: billing.ModeLive, Source: "other-mode", AmountMicros: 999, Kind: "complimentary", RecordedAt: start})
			page := Page{Limit: limit, From: start.Format(time.RFC3339), To: start.AddDate(0, 2, 0).Format(time.RFC3339)}
			seen := map[string]bool{}
			counts := map[string]int{}
			for index := 0; ; index++ {
				view, err := d.Read(t.Context(), page)
				if err != nil {
					t.Fatal(err)
				}
				if len(view.History) == 0 || len(view.History) > limit || view.TotalRows != 41 || view.BalanceMicros != 0 {
					t.Fatalf("page=%+v", view)
				}
				for _, row := range view.History {
					counts[row.Kind]++
					if row.Kind == "usage" {
						day := row.At.Format("2006-01-02")
						if seen[day] || row.Turns != 2 || row.AmountMicros != -246 || row.IncludedAmountMicros != 200 {
							t.Fatalf("duplicate or ungrouped usage=%+v", row)
						}
						seen[day] = true
					} else {
						key := fmt.Sprintf("%s-%d", row.Kind, row.AmountMicros)
						if row.AmountMicros < 5000000 || row.AmountMicros > 5000001 || row.Turns != 0 || seen[key] {
							t.Fatalf("duplicate or merged credit=%+v", row)
						}
						seen[key] = true
					}
				}
				if view.NextCursor == "" {
					break
				}
				if index == 0 {
					hubtest.CreditGrant(t, repository, d.deps.Organization, billingstore.CreditTransaction{Source: "usage:later", AmountMicros: -999, IncludedAmountMicros: 999, Kind: "usage", RecordedAt: start.AddDate(0, 0, 1)})
				}
				page.Cursor = view.NextCursor
				if index > 41 {
					t.Fatal("cursor did not end")
				}
			}
			if counts["usage"] != 35 || counts["purchase"] != 2 || counts["auto_fund"] != 2 || counts["complimentary"] != 2 {
				t.Fatalf("counts=%v", counts)
			}
		})
	}
}
