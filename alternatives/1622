package hubserver

import (
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type reportsRunner struct {
	RunnerID  string                         `json:"runner_id"`
	Name      string                         `json:"name"`
	Sessions  int                            `json:"sessions"`
	Succeeded int                            `json:"succeeded"`
	Duration  operatortool.AnalyticsDuration `json:"duration"`
	Cost      float64                        `json:"cost_usd"`
}

func reportsRunnerTotals(attempts []nativeAnalyticsAttempt, usage map[string][]usageRow) []reportsRunner {
	runners := map[string]*reportsRunner{}
	durations := map[string][]float64{}
	for _, a := range attempts {
		rows := usage[a.AttemptID]
		if len(rows) == 0 || rows[0].RunnerID == "" {
			continue
		}
		id := rows[0].RunnerID
		runner := runners[id]
		if runner == nil {
			runner = &reportsRunner{RunnerID: id, Name: rows[0].RunnerName}
			runners[id] = runner
		}
		runner.Sessions++
		if a.Status == "succeeded" {
			runner.Succeeded++
		}
		if a.Status == "succeeded" || a.Status == "failed" {
			if seconds := closedAttemptSeconds(a.Phases); seconds > 0 {
				durations[id] = append(durations[id], seconds)
			}
		}
		for _, u := range rows {
			runner.Cost += u.Cost
		}
	}
	out := make([]reportsRunner, 0, len(runners))
	for id, runner := range runners {
		runner.Duration = sampledDuration(durations[id])
		out = append(out, *runner)
	}
	slices.SortFunc(out, func(a, b reportsRunner) int {
		if a.Cost != b.Cost {
			if a.Cost > b.Cost {
				return -1
			}
			return 1
		}
		return strings.Compare(a.RunnerID, b.RunnerID)
	})
	return out
}
