package hubserver

import (
	"fmt"
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
		name    string
		partial bool
	}{{name: "complete history"}, {name: "clipped history", partial: true}} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := diagnosticstore.Snapshot{Partial: test.partial, ResidencePartial: test.partial, Attempts: 3, Issues: []diagnosticstore.Issue{{ProjectID: "project", WorkItemID: "first", State: "Done", CreatedAt: from.Add(-time.Hour)}, {ProjectID: "project", WorkItemID: "second", State: "Done", CreatedAt: from.Add(75 * time.Minute)}}, Events: []eventstore.Event{{ProjectID: "project", WorkItemID: "first", Type: "workflow.transitioned", FromState: "Todo", ToState: "Merging", RecordedAt: from.Add(30 * time.Minute)}, {ProjectID: "project", WorkItemID: "first", Type: "workflow.transitioned", FromState: "Merging", ToState: "Done", RecordedAt: from.Add(90 * time.Minute)}, {ProjectID: "project", WorkItemID: "second", Type: "workflow.transitioned", FromState: "Todo", ToState: "Done", RecordedAt: from.Add(3 * time.Hour)}}, Leases: []diagnosticstore.Lease{{AcquiredAt: from.Add(30 * time.Minute), FinishedAt: from.Add(90 * time.Minute)}}}
			report := projectTenantDiagnostics(snapshot, operatortool.AnalyticsWindow{From: from, To: from.Add(2 * time.Hour), Bucket: time.Hour})
			if report.Partial != test.partial || report.MergeEntered != 1 || report.MergeLanded != 1 || report.MergeWaitP50 == nil || *report.MergeWaitP50 != 3600 {
				t.Fatalf("historical merge report = %+v", report)
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
			if report.Coverage[0].Observed == nil || *report.Coverage[0].Observed != 2 || report.Coverage[1].Observed == nil || *report.Coverage[1].Observed != 3 {
				t.Fatalf("recorded history coverage = %+v", report.Coverage)
			}
		})
	}
}

func BenchmarkTenantDiagnosticsNinetyDaysTenThousandEntities(b *testing.B) {
	from := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	snapshot := diagnosticstore.Snapshot{Issues: make([]diagnosticstore.Issue, 10000), Events: make([]eventstore.Event, 10000), Leases: make([]diagnosticstore.Lease, 10000)}
	for index := range snapshot.Issues {
		id := tracker.NativeWorkItemID(fmt.Sprintf("item_%d", index))
		snapshot.Issues[index] = diagnosticstore.Issue{ProjectID: "project", WorkItemID: id, State: "Done", CreatedAt: from.Add(-time.Hour)}
		snapshot.Events[index] = eventstore.Event{ProjectID: "project", WorkItemID: id, Type: "workflow.transitioned", FromState: "Todo", ToState: "Done", RecordedAt: from.Add(time.Hour)}
		snapshot.Leases[index] = diagnosticstore.Lease{AcquiredAt: from.Add(30 * time.Minute), FinishedAt: from.Add(90*24*time.Hour - 30*time.Minute)}
	}
	b.ReportAllocs()
	for b.Loop() {
		report := projectTenantDiagnostics(snapshot, operatortool.AnalyticsWindow{From: from, To: from.Add(90 * 24 * time.Hour), Bucket: time.Hour})
		if len(report.Capacity) != 2160 || report.Capacity[1000].Slots != 10000 {
			b.Fatal("maximum diagnostic population is incomplete")
		}
	}
}
