package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubhealth"
)

const (
	reportKindInfrastructure = "infrastructure"
	reportKindIssue          = "issue"
	reportKindUnknown        = "unknown"
)

type reportClassTotal struct {
	Class              string     `json:"class"`
	Kind               string     `json:"kind"`
	Count              int        `json:"count"`
	Issues             int        `json:"issues"`
	Cost               float64    `json:"cost_usd"`
	WorkItems          []string   `json:"work_items,omitempty"`
	FirstSeen          *time.Time `json:"first_seen,omitempty"`
	LastSeen           *time.Time `json:"last_seen,omitempty"`
	TrackingWorkItemID string     `json:"tracking_work_item_id,omitempty"`
	itemCounts         map[string]int
}

type reportClassRule struct {
	class    hubanalytics.Class
	contains []string
}

var reportNotClassified = hubanalytics.Class{Name: "Not classified", Kind: reportKindUnknown}

var reworkClassRules = []reportClassRule{
	{hubanalytics.Class{Name: "Review findings to address", Kind: reportKindIssue}, []string{"review conversations", "changes requested", "review finding"}},
	{hubanalytics.Class{Name: "Landing refused: protected base", Kind: reportKindInfrastructure}, []string{"base protected", "protected base"}},
	{hubanalytics.Class{Name: "Gate failed before landing", Kind: reportKindIssue}, []string{"gate failed"}},
	{hubanalytics.Class{Name: "Base moved after validation", Kind: reportKindInfrastructure}, []string{"base moved", "base branch changed"}},
	{hubanalytics.Class{Name: "Conflict on landing", Kind: reportKindIssue}, []string{"conflict"}},
	{hubanalytics.Class{Name: "Finished without a change", Kind: reportKindIssue}, []string{"without work product", "no work product"}},
}

var failureClassRules = []reportClassRule{
	{hubanalytics.Class{Name: "Review conversations unresolved at merge", Kind: reportKindIssue}, []string{"review conversations"}},
	{hubanalytics.Class{Name: "Landing refused: protected base", Kind: reportKindInfrastructure}, []string{"base protected", "protected base"}},
	{hubanalytics.Class{Name: "Conflict on landing", Kind: reportKindIssue}, []string{"landing refused: conflict"}},
	{hubanalytics.Class{Name: "Finalization gate failed", Kind: reportKindIssue}, []string{"source finalization gate failed"}},
	{hubanalytics.Class{Name: "Merge resolution gate failed", Kind: reportKindIssue}, []string{"merge resolution gate failed"}},
	{hubanalytics.Class{Name: "Landing gate failed", Kind: reportKindIssue}, []string{"landing gate failed"}},
	{hubanalytics.Class{Name: "Worker processes left running after stop", Kind: reportKindInfrastructure}, []string{"process reap failed", "processes remained after"}},
	{hubanalytics.Class{Name: "Hub request timed out", Kind: reportKindInfrastructure}, []string{"hub unavailable", "hub request timed out"}},
	{hubanalytics.Class{Name: "Hub request failed", Kind: reportKindInfrastructure}, []string{"hub request failed"}},
	{hubanalytics.Class{Name: "Workspace preparation failed", Kind: reportKindInfrastructure}, []string{"workspace preparation failed", "commit squash:"}},
	{hubanalytics.Class{Name: "Agent turn interrupted", Kind: reportKindInfrastructure}, []string{"interrupted turn"}},
	{hubanalytics.Class{Name: "Agent turn timed out", Kind: reportKindInfrastructure}, []string{"worker cancellation: deadline exceeded", "agent turn: context deadline exceeded"}},
	{hubanalytics.Class{Name: "Instance lock held", Kind: reportKindInfrastructure}, []string{"instance lock is held"}},
	{hubanalytics.Class{Name: "Finished without a change", Kind: reportKindIssue}, []string{"without work product", "no work product"}},
}

func classifyReworkReason(fromState, reason string) hubanalytics.Class {
	if fromState == "Human Review" {
		return reworkClassRules[0].class
	}
	return matchReportClass(reworkClassRules, reason)
}

func classifyFailure(text string) hubanalytics.Class {
	return matchReportClass(failureClassRules, text)
}

func matchReportClass(rules []reportClassRule, text string) hubanalytics.Class {
	line := strings.ReplaceAll(strings.ToLower(hubhealth.FailureFirstLine(text)), "_", " ")
	for _, rule := range rules {
		if slices.ContainsFunc(rule.contains, func(fragment string) bool { return strings.Contains(line, fragment) }) {
			return rule.class
		}
	}
	return reportNotClassified
}

type reportClassAccumulator struct {
	totals map[hubanalytics.Class]*reportClassTotal
	issues map[reportClassIssue]bool
}

type reportClassIssue struct {
	class    hubanalytics.Class
	workItem string
}

func newReportClassAccumulator() *reportClassAccumulator {
	return &reportClassAccumulator{totals: map[hubanalytics.Class]*reportClassTotal{}, issues: map[reportClassIssue]bool{}}
}

func (a *reportClassAccumulator) add(class hubanalytics.Class, workItem string, cost float64) {
	total := a.totals[class]
	if total == nil {
		total = &reportClassTotal{Class: class.Name, Kind: class.Kind}
		a.totals[class] = total
	}
	total.Count++
	total.Cost += cost
	if workItem != "" && !a.issues[reportClassIssue{class, workItem}] {
		a.issues[reportClassIssue{class, workItem}] = true
		total.Issues++
	}
}

func (a *reportClassAccumulator) addCost(class hubanalytics.Class, cost float64) {
	if total := a.totals[class]; total != nil {
		total.Cost += cost
	}
}

func (a *reportClassAccumulator) result() []reportClassTotal {
	out := make([]reportClassTotal, 0, len(a.totals))
	for _, total := range a.totals {
		out = append(out, *total)
	}
	slices.SortFunc(out, func(x, y reportClassTotal) int {
		if (x.Class == reportNotClassified.Name) != (y.Class == reportNotClassified.Name) {
			if x.Class == reportNotClassified.Name {
				return 1
			}
			return -1
		}
		if x.Count != y.Count {
			return y.Count - x.Count
		}
		return strings.Compare(x.Class, y.Class)
	})
	return out
}
