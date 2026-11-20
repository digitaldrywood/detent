package hubserver

import (
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
)

type usageRow = usagereport.Row
type usageReport = usagereport.Report
type usageLimit = usagereport.Limit
type runnerCapacity = usagereport.RunnerCapacity

func usageRangeWindow(p0 string, p1 time.Time) (usagecosts.Window, error) {
	r0, r1 := usagereport.RangeWindow(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}
func usageBusySeconds(p0 string, p1 string, p2 usagecosts.Window) int64 {
	return usagereport.BusySeconds(p0, p1, p2)
}
func buildUsageReport(p0 usagecosts.Window, p1 []usagereport.Row, p2 map[string]usagereport.Limit, p3 map[string]usagereport.RunnerCapacity, p4 UsageConfig) usagereport.Report {
	return usagereport.Build(p0, p1, p2, p3, p4)
}
func usageRound(value float64) float64 { return usagereport.Round(value) }
