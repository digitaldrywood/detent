package reportdata

import (
	"slices"
	"sort"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttemptTiming struct {
	Identity                      agentidentity.Identity
	AttemptID, WorkItemID, Status string
	Phases                        []tracker.NativePhase
}
type Usage struct {
	Input, CachedInput, Output int64
	Cost                       float64
}

func (u Usage) tokens() int64 { return u.Input + u.Output }

type Stage struct {
	Stage         string                         `json:"stage"`
	Model         string                         `json:"model"`
	Effort        string                         `json:"effort"`
	Sessions      int                            `json:"sessions"`
	Succeeded     int                            `json:"succeeded"`
	Duration      operatortool.AnalyticsDuration `json:"duration"`
	Tokens        int64                          `json:"tokens"`
	Input         int64                          `json:"input"`
	Cached        int64                          `json:"cached"`
	Cost          float64                        `json:"cost_usd"`
	Issues        int                            `json:"issues"`
	UsageObserved int                            `json:"usage_observed"`
	Disabled      bool                           `json:"disabled"`
}
type Spend struct {
	WorkItemID string  `json:"work_item_id"`
	Title      string  `json:"title"`
	Cost       float64 `json:"cost_usd"`
	Attempts   int     `json:"attempts"`
	Shipped    bool    `json:"shipped"`
	State      string  `json:"state,omitempty"`
}

func ClosedAttemptSeconds(phases []tracker.NativePhase) float64 {
	var seconds float64
	for _, phase := range phases {
		if !phase.FinishedAt.IsZero() {
			seconds += max(0, phase.FinishedAt.Sub(phase.StartedAt).Seconds())
		}
	}
	return seconds
}
func AttemptTotals(attempts []AttemptTiming, usage map[string][]Usage) ([]Stage, []Spend) {
	stages := map[[3]string]*Stage{}
	durations := map[[3]string][]float64{}
	issues := map[[3]string][]string{}
	spend := map[string]float64{}
	for _, a := range attempts {
		stage := a.Identity.Role
		if stage == "" {
			stage = "Unavailable"
		}
		model := a.Identity.Model()
		if model == "" {
			model = "Unavailable"
		}
		effort := a.Identity.ReasoningEffort.Value
		if effort == "" {
			effort = "Unavailable"
		}
		key := [3]string{stage, model, effort}
		row := stages[key]
		if row == nil {
			row = &Stage{Stage: stage, Model: model, Effort: effort}
			stages[key] = row
		}
		row.Sessions++
		issues[key] = append(issues[key], a.WorkItemID)
		if a.Status == "succeeded" {
			row.Succeeded++
		}
		if a.Status == "succeeded" || a.Status == "failed" {
			seconds := ClosedAttemptSeconds(a.Phases)
			if seconds > 0 {
				durations[key] = append(durations[key], seconds)
			}
		}
		if len(usage[a.AttemptID]) > 0 {
			row.UsageObserved++
		}
		for _, u := range usage[a.AttemptID] {
			row.Tokens += u.tokens()
			row.Input += u.Input
			row.Cached += u.CachedInput
			row.Cost += u.Cost
			spend[a.WorkItemID] += u.Cost
		}
	}
	result := []Stage{}
	for key, row := range stages {
		row.Duration = SampledDuration(durations[key])
		slices.Sort(issues[key])
		row.Issues = len(slices.Compact(issues[key]))
		result = append(result, *row)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Stage != b.Stage {
			return a.Stage < b.Stage
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Effort < b.Effort
	})
	costs := []Spend{}
	for id, cost := range spend {
		costs = append(costs, Spend{WorkItemID: id, Cost: cost})
	}
	sort.Slice(costs, func(i, j int) bool {
		if costs[i].Cost == costs[j].Cost {
			return costs[i].WorkItemID < costs[j].WorkItemID
		}
		return costs[i].Cost > costs[j].Cost
	})
	return result, costs
}
