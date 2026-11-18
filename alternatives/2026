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
