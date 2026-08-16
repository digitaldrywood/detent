package web_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

const projectStatePerformanceDeadline = 2 * time.Second

func BenchmarkProjectStateAPIWarmStoreChangingSnapshot(b *testing.B) {
	server, snapshotHub, snapshot := newProjectStatePerformanceServer(b, 5000, 2600)

	warm := projectStatePerformanceRequest(b.Context(), server, 0)
	if warm.Code != http.StatusOK {
		b.Fatalf("warm status = %d, want %d; body = %s", warm.Code, http.StatusOK, warm.Body.String())
	}
	responseBytes := warm.Body.Len()
	b.ResetTimer()
	index := 0
	for b.Loop() {
		index++
		snapshot.GeneratedAt = snapshot.GeneratedAt.Add(time.Second)
		if err := snapshotHub.Publish(snapshot); err != nil {
			b.Fatalf("Publish() error = %v", err)
		}
		recorder := projectStatePerformanceRequest(b.Context(), server, index)
		if recorder.Code != http.StatusOK {
			b.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(responseBytes), "bytes/response")
}

func TestProjectStateAPIRepresentativeDataCompletesBeforeDeadline(t *testing.T) {
	server, snapshotHub, snapshot := newProjectStatePerformanceServer(t, 1000, 500)

	warm := projectStatePerformanceRequest(t.Context(), server, 0)
	if warm.Code != http.StatusOK {
		t.Fatalf("warm status = %d, want %d; body = %s", warm.Code, http.StatusOK, warm.Body.String())
	}
	snapshot.GeneratedAt = snapshot.GeneratedAt.Add(time.Second)
	if err := snapshotHub.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), projectStatePerformanceDeadline)
	defer cancel()
	recorder := projectStatePerformanceRequest(ctx, server, 1)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("project state request exceeded %s", projectStatePerformanceDeadline)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
}

func newProjectStatePerformanceServer(
	tb testing.TB,
	laneEvents int,
	activeEvents int,
) (*web.Server, *hub.Hub[telemetry.Snapshot], telemetry.Snapshot) {
	tb.Helper()
	ctx := context.Background()
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(tb.TempDir(), "detent.db"),
	})
	if err != nil {
		tb.Fatalf("store.Open() error = %v", err)
	}
	tb.Cleanup(func() {
		if err := backend.Close(); err != nil {
			tb.Fatalf("Close() error = %v", err)
		}
	})

	now := time.Date(2026, 8, 15, 22, 16, 27, 0, time.UTC)
	seedProjectStatePerformanceEvents(tb, ctx, backend, now, laneEvents, activeEvents)
	snapshot := benchmarkProjectStateSnapshot(now)
	snapshotHub := hub.New[telemetry.Snapshot]()
	if err := snapshotHub.Publish(snapshot); err != nil {
		tb.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, web.Dependencies{
		Hub:       snapshotHub,
		Store:     backend,
		Registry:  project.NewRegistry(),
		Connector: connectorProbe{name: "memory"},
	})
	if err != nil {
		tb.Fatalf("NewServer() error = %v", err)
	}
	return server, snapshotHub, snapshot
}

func projectStatePerformanceRequest(ctx context.Context, server *web.Server, index int) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/projects/detent/state", nil)
	request.RemoteAddr = fmt.Sprintf("[2001:db8::%x]:1234", index+1)
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func seedProjectStatePerformanceEvents(
	tb testing.TB,
	ctx context.Context,
	backend store.Store,
	now time.Time,
	laneEvents int,
	activeEvents int,
) {
	tb.Helper()

	for index := range laneEvents {
		finishedAt := now.Add(-time.Duration(index%720) * time.Hour)
		issueIndex := index % activeEvents
		_, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			IssueID:         "issue-" + strconv.Itoa(issueIndex),
			Identifier:      "digitaldrywood/detent#" + strconv.Itoa(issueIndex+1),
			PhaseType:       store.WorkflowPhaseTypeLane,
			PhaseName:       "In Progress",
			Status:          "completed",
			StartedAt:       finishedAt.Add(-10 * time.Minute),
			FinishedAt:      finishedAt,
			DurationSeconds: 600,
		})
		if err != nil {
			tb.Fatalf("RecordWorkflowPhaseEvent(lane %d) error = %v", index, err)
		}
	}
	for index := range activeEvents {
		finishedAt := now.Add(-time.Duration(index%720) * time.Hour)
		_, err := backend.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{
			ProjectID:       "detent",
			IssueID:         "issue-" + strconv.Itoa(index),
			Identifier:      "digitaldrywood/detent#" + strconv.Itoa(index+1),
			PhaseType:       store.WorkflowPhaseTypeAgentSession,
			PhaseName:       "agent_active",
			Status:          "completed",
			StartedAt:       finishedAt.Add(-8 * time.Minute),
			FinishedAt:      finishedAt.Add(-5 * time.Minute),
			DurationSeconds: 180,
			Turns:           4,
			TotalTokens:     12000,
			EndpointFamily:  "codex",
		})
		if err != nil {
			tb.Fatalf("RecordWorkflowPhaseEvent(active %d) error = %v", index, err)
		}
	}
}

func benchmarkProjectStateSnapshot(now time.Time) telemetry.Snapshot {
	const workAttempts = 300
	snapshot := telemetry.Snapshot{
		GeneratedAt: now,
		Project:     telemetry.Project{ID: "detent", DisplayName: "Detent"},
		Projects: []telemetry.ProjectSnapshot{{
			Project: telemetry.Project{ID: "detent", DisplayName: "Detent"},
		}},
		WorkAttempts: make([]telemetry.WorkAttempt, 0, workAttempts),
	}
	for index := range workAttempts {
		snapshot.WorkAttempts = append(snapshot.WorkAttempts, telemetry.WorkAttempt{
			AttemptID:      int64(index + 1),
			ProjectID:      "detent",
			IssueID:        "issue-" + strconv.Itoa(index),
			Identifier:     "digitaldrywood/detent#" + strconv.Itoa(index+1),
			IssueURL:       "https://github.com/digitaldrywood/detent/issues/" + strconv.Itoa(index+1),
			Repo:           "digitaldrywood/detent",
			WorkerType:     "codex",
			WorkerHost:     "corys-mac-studio",
			Lane:           "In Progress",
			AttemptNumber:  1,
			Status:         "completed",
			StartedAt:      now.Add(-time.Hour),
			CompletedAt:    &now,
			TerminalState:  "completed",
			StatusMessage:  strings.Repeat("representative state payload ", 4),
			CurrentCommand: "make check",
			NextAction:     "awaiting promotion",
		})
	}
	return snapshot
}
