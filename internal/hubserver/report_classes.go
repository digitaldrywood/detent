package hubserver

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatorfailure"
)

const (
	reportKindInfrastructure = "infrastructure"
	reportKindIssue          = "issue"
	reportKindUnknown        = "unknown"
)

type reportClass struct {
	Name string
	Kind string
}

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
	class    reportClass
	contains []string
}

var reportNotClassified = reportClass{Name: "Not classified", Kind: reportKindUnknown}

var reworkClassRules = []reportClassRule{
	{reportClass{"Review findings to address", reportKindIssue}, []string{"review conversations", "changes requested", "review finding"}},
	{reportClass{"Landing refused: protected base", reportKindInfrastructure}, []string{"base protected", "protected base"}},
	{reportClass{"Gate failed before landing", reportKindIssue}, []string{"gate failed"}},
	{reportClass{"Base moved after validation", reportKindInfrastructure}, []string{"base moved", "base branch changed"}},
	{reportClass{"Conflict on landing", reportKindIssue}, []string{"conflict"}},
	{reportClass{"Finished without a change", reportKindIssue}, []string{"without work product", "no work product"}},
}

var failureClassRules = []reportClassRule{
	{reportClass{"Review conversations unresolved at merge", reportKindIssue}, []string{"review conversations"}},
	{reportClass{"Landing refused: protected base", reportKindInfrastructure}, []string{"base protected", "protected base"}},
	{reportClass{"Conflict on landing", reportKindIssue}, []string{"landing refused: conflict"}},
	{reportClass{"Finalization gate failed", reportKindIssue}, []string{"source finalization gate failed"}},
	{reportClass{"Merge resolution gate failed", reportKindIssue}, []string{"merge resolution gate failed"}},
	{reportClass{"Landing gate failed", reportKindIssue}, []string{"landing gate failed"}},
	{reportClass{"Worker processes left running after stop", reportKindInfrastructure}, []string{"process reap failed", "processes remained after"}},
	{reportClass{"Hub request timed out", reportKindInfrastructure}, []string{"hub unavailable", "hub request timed out"}},
	{reportClass{"Hub request failed", reportKindInfrastructure}, []string{"hub request failed"}},
	{reportClass{"Workspace preparation failed", reportKindInfrastructure}, []string{"workspace preparation failed", "commit squash:"}},
	{reportClass{"Agent turn interrupted", reportKindInfrastructure}, []string{"interrupted turn"}},
	{reportClass{"Agent turn timed out", reportKindInfrastructure}, []string{"worker cancellation: deadline exceeded", "agent turn: context deadline exceeded"}},
	{reportClass{"Instance lock held", reportKindInfrastructure}, []string{"instance lock is held"}},
	{reportClass{"Finished without a change", reportKindIssue}, []string{"without work product", "no work product"}},
}

func classifyReworkReason(fromState, reason string) reportClass {
	if fromState == "Human Review" {
		return reworkClassRules[0].class
	}
	return matchReportClass(reworkClassRules, reason)
}

func classifyFailure(text string) reportClass {
	return matchReportClass(failureClassRules, text)
}

func matchReportClass(rules []reportClassRule, text string) reportClass {
	line := strings.ReplaceAll(strings.ToLower(operatorfailure.FirstLine(text)), "_", " ")
	for _, rule := range rules {
		if slices.ContainsFunc(rule.contains, func(fragment string) bool { return strings.Contains(line, fragment) }) {
			return rule.class
		}
	}
	return reportNotClassified
}

type reportClassAccumulator struct {
	totals map[reportClass]*reportClassTotal
	issues map[reportClassIssue]bool
}

type reportClassIssue struct {
	class    reportClass
	workItem string
}

func newReportClassAccumulator() *reportClassAccumulator {
	return &reportClassAccumulator{totals: map[reportClass]*reportClassTotal{}, issues: map[reportClassIssue]bool{}}
}

func (a *reportClassAccumulator) add(class reportClass, workItem string, cost float64) {
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

func (a *reportClassAccumulator) addCost(class reportClass, cost float64) {
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
