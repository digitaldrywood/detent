package reports

import (
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
)

const (
	reportKindInfrastructure = "infrastructure"
	reportKindIssue          = "issue"
	reportKindUnknown        = "unknown"
)

type ReportClass = reportstore.Class

type ReportClassTotal struct {
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

var reportNotClassified = reportstore.NotClassified

func classifyReworkReason(fromState, reason string) ReportClass {
	return reportstore.ClassifyReworkReason(fromState, reason)
}
func ClassifyFailure(text string) ReportClass { return reportstore.ClassifyFailure(text) }

type reportClassAccumulator struct {
	totals map[ReportClass]*ReportClassTotal
	Issues map[reportClassIssue]bool `json:"-"`
}

type reportClassIssue struct {
	class    ReportClass
	workItem string
}

func newReportClassAccumulator() *reportClassAccumulator {
	return &reportClassAccumulator{totals: map[ReportClass]*ReportClassTotal{}, Issues: map[reportClassIssue]bool{}}
}

func (a *reportClassAccumulator) add(class ReportClass, workItem string, cost float64) {
	total := a.totals[class]
	if total == nil {
		total = &ReportClassTotal{Class: class.Name, Kind: class.Kind}
		a.totals[class] = total
	}
	total.Count++
	total.Cost += cost
	if workItem != "" && !a.Issues[reportClassIssue{class, workItem}] {
		a.Issues[reportClassIssue{class, workItem}] = true
		total.Issues++
	}
}

func (a *reportClassAccumulator) addCost(class ReportClass, cost float64) {
	if total := a.totals[class]; total != nil {
		total.Cost += cost
	}
}

func (a *reportClassAccumulator) result() []ReportClassTotal {
	out := make([]ReportClassTotal, 0, len(a.totals))
	for _, total := range a.totals {
		out = append(out, *total)
	}
	slices.SortFunc(out, func(x, y ReportClassTotal) int {
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
