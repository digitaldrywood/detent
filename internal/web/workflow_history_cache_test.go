package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

type workflowHistoryTestStore struct {
	store.Store
	revision      atomic.Int64
	reports       atomic.Int64
	cycles        atomic.Int64
	cycleError    bool
	evidence      atomic.Int64
	revisionError bool
	reportError   bool
	onReport      func()
}

func (s *workflowHistoryTestStore) WorkflowHistoryRevision(context.Context) (int64, error) {
	if s.revisionError {
		return 0, errors.New("revision unavailable")
	}
	return s.revision.Load(), nil
}
func (s *workflowHistoryTestStore) RuntimeEvidence(context.Context, store.RuntimeEvidenceQuery) (store.RuntimeEvidence, error) {
	s.evidence.Add(1)
	return store.RuntimeEvidence{Healthy: true}, nil
}
func (s *workflowHistoryTestStore) WorkflowMetricsReport(_ context.Context, q store.WorkflowMetricsQuery) (store.WorkflowMetricsReport, error) {
	s.reports.Add(1)
	if s.onReport != nil {
		s.onReport()
	}
	if s.reportError {
		return store.WorkflowMetricsReport{}, errors.New("report unavailable")
	}
	return store.WorkflowMetricsReport{Lanes: []store.WorkflowPhaseMetric{{ProjectID: q.ProjectID, PhaseName: "In Progress", Count: 1, AverageSeconds: s.revision.Load()}}}, nil
}

func (s *workflowHistoryTestStore) CycleTimeReport(context.Context) (store.CycleTimeReport, error) {
	s.cycles.Add(1)
	if s.cycleError {
		return store.CycleTimeReport{}, errors.New("cycle report unavailable")
	}
	return store.CycleTimeReport{
		AverageSeconds: s.revision.Load() + 42,
		Issues:         []store.CycleTimeIssue{{Key: "completed", DurationSeconds: 60, Sessions: 2}},
		Buckets:        []store.CycleTimeBucket{{Label: "short", Count: 1}},
	}, nil
}

func TestWorkflowHistoryCacheCadence(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		elapsed, timeShift time.Duration
		project            string
		changed            bool
		want               int64
	}{
		{name: "heartbeat", elapsed: time.Second, timeShift: time.Second, want: 6},
		{name: "bounded wall freshness", elapsed: 30 * time.Second, want: 12},
		{name: "moving window", timeShift: 30 * time.Second, want: 12},
		{name: "clock moves backward", elapsed: -time.Second, want: 12},
		{name: "window moves backward", timeShift: -time.Second, want: 12},
		{name: "correction", changed: true, want: 12},
		{name: "project scope", project: "other", want: 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &workflowHistoryTestStore{}
			now := base
			server := &Server{store: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
			snapshot := telemetry.Snapshot{Seq: 1, GeneratedAt: base}
			firstCycle, _ := server.snapshotCycleTime(t.Context(), snapshot.GeneratedAt)
			first := server.snapshotWorkflowMetrics(t.Context(), snapshot)
			if !first.Available {
				t.Fatal("history unavailable")
			}
			now = base.Add(tt.elapsed)
			snapshot.GeneratedAt = base.Add(tt.timeShift)
			snapshot.Seq++
			snapshot.Project.ID = tt.project
			snapshot.Running = []telemetry.Running{{Issue: telemetry.Issue{ID: "live"}, RuntimeSeconds: 42}}
			if tt.changed {
				backend.revision.Add(1)
			}
			cycle, _ := server.snapshotCycleTime(t.Context(), snapshot.GeneratedAt)
			got := server.snapshotWorkflowMetrics(t.Context(), snapshot)
			if backend.reports.Load() != tt.want {
				t.Fatalf("reports = %d, want %d", backend.reports.Load(), tt.want)
			}
			if backend.evidence.Load() != tt.want/6 {
				t.Fatalf("evidence queries = %d", backend.evidence.Load())
			}
			wantCycles := tt.want / 6
			if tt.project != "" {
				wantCycles = 1
			}
			if backend.cycles.Load() != wantCycles {
				t.Fatalf("cycle reports = %d, want %d", backend.cycles.Load(), wantCycles)
			}
			if !firstCycle.Available || !cycle.Available || firstCycle.AverageSeconds != 42 || cycle.AverageSeconds != backend.revision.Load()+42 || len(cycle.Issues) != 1 || cycle.Issues[0].DurationSeconds != 60 || cycle.Issues[0].Sessions != 2 || len(cycle.Buckets) != 1 || cycle.Buckets[0].Count != 1 {
				t.Fatalf("cycle projection changed: first=%+v current=%+v", firstCycle, cycle)
			}
			if got.ActiveBottleneck.IssueID != "live" || got.ActiveBottleneck.Seconds != 42 {
				t.Fatalf("stale operational evidence: %#v", got.ActiveBottleneck)
			}
			if got.Windows[0].Lanes[0].ProjectID != tt.project {
				t.Fatal("project scope leaked")
			}
			wantTo := base
			if tt.want == 12 {
				wantTo = snapshot.GeneratedAt
			}
			if !got.Windows[0].To.Equal(wantTo) {
				t.Fatalf("window timestamp = %s, want %s", got.Windows[0].To, wantTo)
			}
		})
	}
}

func TestWorkflowHistoryConcurrentClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backend := &workflowHistoryTestStore{}
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		backend.onReport = func() { once.Do(func() { close(started); <-release }) }
		server := &Server{store: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
		snapshot := telemetry.Snapshot{GeneratedAt: time.Now()}
		var wg sync.WaitGroup
		for range 12 {
			wg.Go(func() {
				if !server.snapshotWorkflowMetrics(t.Context(), snapshot).Available {
					t.Error("history unavailable")
				}
			})
		}
		<-started
		synctest.Wait()
		close(release)
		wg.Wait()
		if backend.reports.Load() != 6 || backend.evidence.Load() != 1 || backend.cycles.Load() != 1 {
			t.Fatalf("queries: reports=%d evidence=%d cycles=%d", backend.reports.Load(), backend.evidence.Load(), backend.cycles.Load())
		}
	})
}

func TestWorkflowHistoryCacheFailureAndConcurrentMutation(t *testing.T) {
	for _, mode := range []string{"report failure", "cycle failure", "revision failure", "mutation during load", "canceled load"} {
		t.Run(mode, func(t *testing.T) {
			backend := &workflowHistoryTestStore{}
			server := &Server{store: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
			snapshot := telemetry.Snapshot{GeneratedAt: time.Now()}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "report failure":
				backend.reportError = true
			case "cycle failure":
				backend.cycleError = true
			case "revision failure":
				backend.revisionError = true
			case "mutation during load":
				backend.onReport = func() { backend.revision.Add(1) }
			case "canceled load":
				backend.onReport = cancel
			}
			first := server.workflowHistory.get(ctx, backend, "", snapshot.GeneratedAt, server.now, server.loadWorkflowHistory)
			if mode == "cycle failure" && (!first.metrics.Available || first.cycleTime.Available || first.cycleTime.DegradedReason != "cycle-time query failed") {
				t.Fatalf("cycle failure changed independent reports: %+v", first)
			}
			before := backend.reports.Load()
			backend.reportError = false
			backend.cycleError = false
			backend.revisionError = false
			backend.onReport = nil
			if !server.snapshotWorkflowMetrics(t.Context(), snapshot).Available {
				t.Fatal("retry did not recover")
			}
			if backend.reports.Load() != before+6 {
				t.Fatal("invalid load was cached")
			}
		})
	}
}

func TestWorkflowHistoryWaitCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backend := &workflowHistoryTestStore{}
		release := make(chan struct{})
		backend.onReport = func() { <-release }
		server := &Server{store: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
		snapshot := telemetry.Snapshot{GeneratedAt: time.Now()}
		var wg sync.WaitGroup
		wg.Go(func() { server.snapshotWorkflowMetrics(t.Context(), snapshot) })
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan telemetry.WorkflowMetrics, 1)
		go func() { done <- server.snapshotWorkflowMetrics(ctx, snapshot) }()
		synctest.Wait()
		cancel()
		if got := <-done; got.Available || got.DegradedReason == "" {
			t.Fatal("canceled waiter returned history")
		}
		close(release)
		wg.Wait()
	})
}

func TestSnapshotEnrichmentSkipsSupersededClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newSnapshotEnrichmentCache()
		release := make(chan struct{})
		var calls atomic.Int64
		enrich := func(_ context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot {
			if calls.Add(1) == 1 {
				<-release
			}
			return snapshot
		}
		base := telemetry.Snapshot{Seq: 1, GeneratedAt: time.Now()}
		var wg sync.WaitGroup
		wg.Go(func() { cache.enrich(t.Context(), base, enrich) })
		synctest.Wait()
		for seq := uint64(2); seq <= 10; seq++ {
			snapshot := base
			snapshot.Seq = seq
			wg.Go(func() { cache.enrich(t.Context(), snapshot, enrich) })
		}
		synctest.Wait()
		close(release)
		wg.Wait()
		if calls.Load() != 2 {
			t.Fatalf("enrich calls = %d, want 2", calls.Load())
		}
		if got := cache.enrich(t.Context(), base, enrich); got.Seq != 10 {
			t.Fatalf("stale client regressed cache to %d", got.Seq)
		}
	})
}

func TestSnapshotEnrichmentHistoryInvalidation(t *testing.T) {
	for _, tt := range []struct {
		name     string
		revision int64
		elapsed  time.Duration
		want     int
	}{
		{name: "identical snapshot", want: 1},
		{name: "earlier request clock sample", elapsed: -time.Nanosecond, want: 1},
		{name: "earlier request with corrected history", elapsed: -time.Nanosecond, revision: 1, want: 2},
		{name: "corrected history", revision: 1, want: 2},
		{name: "before freshness deadline", elapsed: workflowHistoryFreshness - time.Nanosecond, want: 1},
		{name: "bounded freshness", elapsed: workflowHistoryFreshness, want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cache := newSnapshotEnrichmentCache()
			now := time.Now()
			calls := 0
			enrich := func(_ context.Context, snapshot telemetry.Snapshot) telemetry.Snapshot { calls++; return snapshot }
			snapshot := telemetry.Snapshot{Seq: 1, GeneratedAt: now}
			cache.enrichVersion(t.Context(), snapshot, 0, now, enrich)
			cache.enrichVersion(t.Context(), snapshot, tt.revision, now.Add(tt.elapsed), enrich)
			if calls != tt.want {
				t.Fatalf("calls = %d, want %d", calls, tt.want)
			}
		})
	}
}

func TestWorkflowHistoryScopeEviction(t *testing.T) {
	backend := &workflowHistoryTestStore{}
	now := time.Now()
	server := &Server{store: backend, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }}
	for i := range maxWorkflowHistoryScopes + 1 {
		snapshot := telemetry.Snapshot{GeneratedAt: now}
		snapshot.Project.ID = string(rune('a' + i))
		server.snapshotWorkflowMetrics(t.Context(), snapshot)
		now = now.Add(time.Millisecond)
	}
	if len(server.workflowHistory.entries) != maxWorkflowHistoryScopes {
		t.Fatalf("cached scopes = %d", len(server.workflowHistory.entries))
	}
	if _, ok := server.workflowHistory.entries["a"]; ok {
		t.Fatal("oldest scope was retained")
	}
}
