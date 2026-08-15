package web_test

import (
	"context"
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

func BenchmarkProjectStateAPIWarmCache(b *testing.B) {
	ctx := context.Background()
	backend, err := store.Open(ctx, store.Config{
		Backend: store.BackendSQLite,
		Path:    filepath.Join(b.TempDir(), "detent.db"),
	})
	if err != nil {
		b.Fatalf("store.Open() error = %v", err)
	}
	b.Cleanup(func() {
		if err := backend.Close(); err != nil {
			b.Fatalf("Close() error = %v", err)
		}
	})

	now := time.Date(2026, 8, 15, 22, 16, 27, 0, time.UTC)
	seedBenchmarkWorkflowEvents(b, backend, now)
	snapshot := benchmarkProjectStateSnapshot(now)
	snapshotHub := hub.New[telemetry.Snapshot]()
	if err := snapshotHub.Publish(snapshot); err != nil {
		b.Fatalf("Publish() error = %v", err)
	}
	server, err := web.NewServer(web.Config{}, web.Dependencies{
		Hub:       snapshotHub,
		Store:     backend,
		Registry:  project.NewRegistry(),
		Connector: connectorProbe{name: "memory"},
	})
	if err != nil {
		b.Fatalf("NewServer() error = %v", err)
	}

	warm := httptest.NewRecorder()
	server.Handler().ServeHTTP(warm, httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/v1/projects/detent/state", nil))
	if warm.Code != http.StatusOK {
		b.Fatalf("warm status = %d, want %d; body = %s", warm.Code, http.StatusOK, warm.Body.String())
	}
	responseBytes := warm.Body.Len()
	b.ResetTimer()
	for b.Loop() {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/api/v1/projects/detent/state", nil))
		if recorder.Code != http.StatusOK {
			b.Fatalf("status = %d, want %d; body = %s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(responseBytes), "bytes/response")
}

func seedBenchmarkWorkflowEvents(b *testing.B, backend store.Store, now time.Time) {
	b.Helper()
	const laneEvents = 1670
	const activeEvents = 870

	for index := range laneEvents {
		finishedAt := now.Add(-time.Duration(index%720) * time.Hour)
		issueIndex := index % activeEvents
		_, err := backend.RecordWorkflowPhaseEvent(b.Context(), store.WorkflowPhaseEvent{
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
			b.Fatalf("RecordWorkflowPhaseEvent(lane %d) error = %v", index, err)
		}
	}
	for index := range activeEvents {
		finishedAt := now.Add(-time.Duration(index%720) * time.Hour)
		_, err := backend.RecordWorkflowPhaseEvent(b.Context(), store.WorkflowPhaseEvent{
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
			b.Fatalf("RecordWorkflowPhaseEvent(active %d) error = %v", index, err)
		}
	}
}

func benchmarkProjectStateSnapshot(now time.Time) telemetry.Snapshot {
	const workAttempts = 600
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
