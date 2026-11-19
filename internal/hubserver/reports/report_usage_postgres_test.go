package reports

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

func TestPostgresReportUsageRowsCurrency(t *testing.T) {
	t.Parallel()
	period := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, currency string
		wantError      bool
	}{{"usd", "usd", false}, {"other currency", "eur", true}} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PostgresReportUsageRows([]reportstore.UsageFact{{ProjectID: "example", WorkItemID: "shipped", AttemptID: "attempt_shipped", Period: period, Cost: 3, Currency: test.currency}})
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
