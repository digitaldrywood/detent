package diagnostics_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDiagnosticsWindowReads(t *testing.T) {
	fixtures := tenants(t)
	now := time.Now().UTC().Truncate(time.Minute)
	for _, f := range fixtures {
		item := tracker.NativeWorkItemID(f.organization)
		seedItem(t, f, project, item, "Todo", now.Add(-time.Hour))
		seedEvent(t, f, project, item, "decision", 1, "scheduler.decision", now.Add(-15*time.Minute))
	}
	var windows diagnosticstore.Windows
	for _, f := range fixtures {
		item := tracker.NativeWorkItemID(f.organization)
		read := func(wantState string) {
			t.Helper()
			if err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
				reader := r.(diagnosticstore.Reader)
				projects := []tracker.ProjectID{project}
				from := now.Add(-24 * time.Hour)
				coldStatements := 0
				coldCtx := postgres.WithTransactionTiming(t.Context(), nil, func(time.Time, string) { coldStatements++ })
				want, err := reader.DiagnosticsSnapshot(coldCtx, projects, from, now, now, "")
				if err != nil {
					return err
				}
				if len(want.Issues) != 1 || want.Issues[0].WorkItemID != item || want.Issues[0].State != wantState {
					t.Fatalf("unexpected scoped issues: %+v", want.Issues)
				}
				loads, statements := 0, 0
				ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, query string) {
					statements++
					if strings.Contains(query, "WITH RECURSIVE lease_machines") {
						loads++
					}
				})
				window := diagnosticstore.ReportingWindow{From: from, To: now, Observed: now, Sampled: true}
				for range 4 {
					got, err := windows.Read(ctx, reader, f.organization, 1, projects, window, "")
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got.Snapshot, want) || !got.To.Equal(now) {
						t.Errorf("%s repeated read differs from its PostgreSQL snapshot", f.organization)
					}
					window.Observed = window.Observed.Add(time.Second)
					window.To = window.Observed
				}
				if statements != coldStatements+4 {
					t.Errorf("%s: four requests executed %d statements, want %d for one snapshot and four version checks", f.organization, statements, coldStatements+4)
				}
				if loads != 1 {
					t.Errorf("%s: four identical requests executed %d diagnostics lease reads, want 1", f.organization, loads)
				}
				filtered, err := windows.Read(ctx, reader, f.organization, 1, projects, window, "other-principal")
				if err == nil && (len(filtered.Issues) != 0 || loads != 2) {
					t.Errorf("principal filter returned %d issues after %d lease reads", len(filtered.Issues), loads)
				}
				if err != nil {
					return err
				}
				earlier := diagnosticstore.ReportingWindow{From: from, To: now.Add(-time.Hour), Observed: now}
				prior, err := windows.Read(ctx, reader, f.organization, 1, projects, earlier, "")
				if err == nil && (len(want.Events) != 1 || len(prior.Events) != 0 || loads != 3) {
					t.Errorf("distinct reporting windows returned %d and %d events after %d lease reads", len(want.Events), len(prior.Events), loads)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		read("Todo")
		seedSQL(t, f, "UPDATE issues SET workflow_state_name='In Progress' WHERE organization_id=$1 AND native_id=$2", f.organization, item)
		read("In Progress")
	}
}
