package reports

import (
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
)

type NativeAnalyticsAttemptPopulation struct {
	Items                   []NativeAnalyticsAttempt
	Partial                 bool
	Malformed               bool
	Clipped                 bool
	ContinuationUnavailable bool
}

func reportsAttemptTotals(attempts []NativeAnalyticsAttempt, usage map[string][]usagereport.Row) ([]reportdata.Stage, []reportdata.Spend) {
	values := make([]reportdata.AttemptTiming, len(attempts))
	for n, a := range attempts {
		values[n] = reportdata.AttemptTiming{Identity: a.Identity, AttemptID: a.AttemptID, WorkItemID: a.WorkItemID, Status: a.Status, Phases: a.Phases}
	}
	rows := make(map[string][]reportdata.Usage, len(usage))
	for id, us := range usage {
		for _, u := range us {
			rows[id] = append(rows[id], reportdata.Usage{Input: u.Input, CachedInput: u.CachedInput, Output: u.Output, Cost: u.Cost})
		}
	}
	return reportdata.AttemptTotals(values, rows)
}
