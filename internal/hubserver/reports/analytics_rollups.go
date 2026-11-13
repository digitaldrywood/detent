package reports

import "time"

const (
	analyticsBackfillBatch  = 20
	analyticsBackfillBudget = 2 * time.Second

	analyticsSampleRetention       = 30 * 24 * time.Hour
	analyticsSampleCompactionBatch = 5000
)
