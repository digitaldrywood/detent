package reports

import (
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"

	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestTenantDiagnosticsReconstructsHistoricalTodoAndLeaseOccupancy(t *testing.T) {
	from := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		partial        bool
		missingReceipt bool
		nonMergeDone   bool
	}{{name: "complete history"}, {name: "clipped history", partial: true}, {name: "shipped without receipt", missingReceipt: true}, {name: "non-merge Done is not shipped", nonMergeDone: true}} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := diagnosticstore.Snapshot{Partial: test.partial, ResidencePartial: test.partial, Attempts: 3, Landings: []diagnosticstore.Landing{{ChangeID: "change", WorkItemID: "first", At: from.Add(90 * time.Minute), EnteredAt: new(from.Add(30 * time.Minute))}}, Issues: []diagnosticstore.Issue{{ProjectID: "project", WorkItemID: "first", State: "Done", CreatedAt: from.Add(-time.Hour)}, {ProjectID: "project", WorkItemID: "second", State: "Done", CreatedAt: from.Add(75 * time.Minute)}}, Events: []eventstore.Event{{ProjectID: "project", WorkItemID: "first", Type: "workflow.transitioned", FromState: "Todo", ToState: "Merging", RecordedAt: from.Add(30 * time.Minute)}, {ProjectID: "project", WorkItemID: "first", Type: "workflow.transitioned", FromState: "Merging", ToState: "Done", RecordedAt: from.Add(90 * time.Minute)}, {ProjectID: "project", WorkItemID: "second", Type: "workflow.transitioned", FromState: "Todo", ToState: "Done", RecordedAt: from.Add(3 * time.Hour)}}, Leases: []diagnosticstore.Lease{{AcquiredAt: from.Add(30 * time.Minute), FinishedAt: from.Add(90 * time.Minute)}}}
			if test.missingReceipt {
				snapshot.Landings = nil
			}
			if test.nonMergeDone {
				snapshot.Issues = append(snapshot.Issues, diagnosticstore.Issue{ProjectID: "project", WorkItemID: "manual", State: "Done", CreatedAt: from.Add(-time.Hour)})
				snapshot.Events = append(snapshot.Events, eventstore.Event{ProjectID: "project", WorkItemID: "manual", Type: "workflow.transitioned", FromState: "Todo", ToState: "Done", RecordedAt: from.Add(45 * time.Minute)})
				slices.SortFunc(snapshot.Events, func(a, b eventstore.Event) int { return a.RecordedAt.Compare(b.RecordedAt) })
			}
			report := ProjectTenantDiagnostics(snapshot, operatortool.AnalyticsWindow{From: from, To: from.Add(2 * time.Hour), Bucket: time.Hour})
			if report.Partial != test.partial || report.MergeEntered != 1 || report.MergeLanded != 1 || !test.missingReceipt && (report.MergeWaitP50 == nil || *report.MergeWaitP50 != 3600) || test.missingReceipt && report.MergeWaitP50 != nil {
				t.Fatalf("historical merge report = %+v", report)
			}
			identity := report.Coverage[6]
			observed := 1
			if test.missingReceipt {
				observed = 0
			}
			if identity.Source != "Merge identity" || identity.Observed == nil || *identity.Observed != observed || identity.Total == nil || *identity.Total != 1 {
				t.Fatalf("merge receipt coverage hid shipped outcomes: %+v", identity)
			}
			if len(report.Capacity) != 2 {
				t.Fatalf("capacity buckets = %+v", report.Capacity)
			}
			for index, bucket := range report.Capacity {
				if bucket.Slots != 0.5 {
					t.Errorf("bucket %d occupancy = %g, want 0.5", index, bucket.Slots)
				}
				if test.partial {
					if bucket.Todo != nil {
						t.Errorf("clipped bucket %d invents Todo count %d", index, *bucket.Todo)
					}
				} else if bucket.Todo == nil || *bucket.Todo != index {
					t.Errorf("bucket %d Todo = %v, want %d", index, bucket.Todo, index)
				}
			}
			transitions := 2
			if test.nonMergeDone {
				transitions = 3
			}
			if report.Coverage[0].Observed == nil || *report.Coverage[0].Observed != transitions || report.Coverage[1].Observed == nil || *report.Coverage[1].Observed != 3 {
				t.Fatalf("recorded history coverage = %+v", report.Coverage)
			}
		})
	}
}

func TestDiagnosticsTodoHistory(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	states := map[string]tracker.NativeState{"Todo": {Name: "Todo", Dispatchable: true}, "In Progress": {Name: "In Progress", Dispatchable: true}}
	issue := AnalyticsResidenceIssue{Created: at.Add(-time.Hour), Initial: "Todo", Events: []AnalyticsResidenceEvent{{At: at, Kind: "workflow.transitioned", Data: tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}}}}
	for _, test := range []struct {
		name        string
		at          time.Time
		initial     string
		partial     bool
		clipped     bool
		want        int
		unavailable bool
	}{
		{name: "not yet created", at: at.Add(-2 * time.Hour), initial: "Todo", want: 0},
		{name: "future dispatch cannot change past queue", at: at.Add(-time.Minute), initial: "Todo", want: 1},
		{name: "exclusive bucket end", at: at, initial: "Todo", want: 1},
		{name: "dispatch leaves queue", at: at.Add(time.Minute), initial: "Todo", want: 0},
		{name: "missing initial history", at: at, unavailable: true},
		{name: "incomplete history", at: at, initial: "Todo", partial: true, unavailable: true},
		{name: "clipped population", at: at, initial: "Todo", clipped: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := issue
			input.Initial, input.Partial = test.initial, test.partial
			got := diagnosticsTodoDepth([]AnalyticsResidenceIssue{input}, states, test.clipped, test.at)
			if test.unavailable {
				if got != nil {
					t.Fatalf("queue depth = %d, want unavailable", *got)
				}
				return
			}
			if got == nil || *got != test.want {
				t.Fatalf("queue depth = %v, want %d", got, test.want)
			}
		})
	}
}
