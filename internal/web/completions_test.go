package web

import (
	"bytes"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/coordination"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/store/storetest"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

// Catches a verified programmatic merge disappearing from the board when no
// terminal worker attempt exists, including after runtime state is discarded.
func TestBoardDurableProgrammaticCompletion(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	cfg := store.Config{Backend: store.BackendSQLite, Path: storetest.NewDatabasePath(t)}
	backend, err := store.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if backend != nil {
			if err := backend.Close(); err != nil {
				t.Errorf("store.Close() error = %v", err)
			}
		}
	})
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	write, err := backend.PrepareLaneWrite(t.Context(), "detent", coordination.LaneWrite{Issue: "issue-3618", From: "Merging", To: "Done", Reason: "merge_worker_programmatic_merge", WrittenAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ResolveLaneWrite(t.Context(), write.FenceToken, "applied"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{ProjectID: "detent", IssueID: "issue-3618", Identifier: "digitaldrywood/detent#3618", IssueURL: "https://github.com/digitaldrywood/detent/issues/3618", PhaseType: store.WorkflowPhaseTypeLane, PhaseName: "Done", Status: "entered", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	// A repeated merge receipt retains the original completion time.
	write.WrittenAt = now
	repeat, err := backend.PrepareLaneWrite(t.Context(), "detent", write)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.ResolveLaneWrite(t.Context(), repeat.FenceToken, "applied"); err != nil {
		t.Fatal(err)
	}
	var before []telemetry.Completed
	for _, restart := range []bool{false, true} {
		if restart {
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			backend, err = store.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
		}
		server := &Server{store: backend, now: func() time.Time { return now }, logger: slog.Default()}
		snapshot := server.enrichSnapshot(t.Context(), telemetry.Snapshot{GeneratedAt: now, Project: telemetry.Project{ID: "detent"}})
		if len(snapshot.Shipped) != 1 || !snapshot.Shipped[0].CompletedAt.Equal(at) {
			t.Fatalf("shipped = %+v, want one original merge receipt", snapshot.Shipped)
		}
		if len(snapshot.Completed) != 0 || snapshot.Counts.Completed != 0 || len(snapshot.WorkAttempts) != 0 {
			t.Fatal("durable shipping was mixed with runtime session completion")
		}
		if restart && !reflect.DeepEqual(snapshot.Shipped, before) {
			t.Fatalf("restart changed durable completion projection: before=%+v after=%+v", before, snapshot.Shipped)
		}
		before = snapshot.Shipped
		var html bytes.Buffer
		if err := templates.BoardSnapshot(templates.DashboardData{Snapshot: snapshot, Kanban: templates.KanbanData{States: []string{"Todo", "Done"}, TerminalStates: []string{"Done"}}}).Render(t.Context(), &html); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(html.String(), "Completed digitaldrywood/detent#3618") {
			t.Fatal("board missing durable programmatic completion without a worker attempt")
		}
		figure := regexp.MustCompile(`id="fig-completed"><span[^>]*>1</span>\s+completed · 48h`)
		if !figure.MatchString(html.String()) {
			start := strings.Index(html.String(), `id="fig-completed"`)
			if start < 0 {
				t.Fatal("completion figure is missing")
			}
			t.Fatal("board figure missing one durable completion")
		}
		// Scoped reads cannot reveal a different project's durable completion.
		scoped := projectScopedSnapshotForProject(snapshot, telemetry.Project{ID: "docs"})
		if len(scoped.Shipped) != 0 {
			t.Fatalf("other project's scoped snapshot contains shipping: %+v", scoped.Shipped)
		}
		authorized := operatorScopedSnapshot(snapshot, []string{"docs"})
		if len(authorized.Shipped) != 0 {
			t.Fatal("project authorization leaked durable delivery")
		}
	}
}
