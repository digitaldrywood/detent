package web_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web"
)

type workflowSSECountingStore struct {
	enrichmentQueryCountingStore
	revision atomic.Int64
}

type sseMetricsLogWriter chan string

func (w sseMetricsLogWriter) Write(p []byte) (int, error) {
	select {
	case w <- string(p):
	default:
	}
	return len(p), nil
}

func TestWorkflowHistoryUnchangedSSETickSkipsQueries(t *testing.T) {
	t.Parallel()
	backend := &workflowSSECountingStore{}
	deps := testDeps(t)
	deps.Store = backend
	logs := make(sseMetricsLogWriter, 128)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	server, err := web.NewServer(web.Config{
		SSETickInterval:     time.Hour,
		SSEMetricsInterval:  time.Nanosecond,
		SSEFragmentInterval: -1,
		Logger:              slog.New(slog.NewTextHandler(io.Writer(logs), &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:                 func() time.Time { return now },
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	addr := startWebServer(t, server)
	conn, reader := openRawEventStream(t, addr)
	first := telemetry.Snapshot{Seq: 1, GeneratedAt: now, BoardIssues: []telemetry.Issue{{ID: "issue-1", Title: "Idle card", CurrentLaneAgeSeconds: 100}}}
	if err := deps.Hub.Publish(first); err != nil {
		t.Fatal(err)
	}
	readRawSSEEventNamed(t, conn, reader, "snapshot")
	queries := backend.workflowMetricsCalls.Load()
	if queries == 0 {
		t.Fatal("initial snapshot issued no workflow metrics queries")
	}

	second := first
	second.Seq = 2
	second.GeneratedAt = now.Add(time.Second)
	second.BoardIssues = append([]telemetry.Issue(nil), first.BoardIssues...)
	second.BoardIssues[0].CurrentLaneAgeSeconds++
	if err := deps.Hub.Publish(second); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-logs:
			if !strings.Contains(line, "dashboard sse stream metrics") || !strings.Contains(line, "event=snapshot") || !strings.Contains(line, "skipped_fingerprint=1") {
				continue
			}
			if got := backend.workflowMetricsCalls.Load(); got != queries {
				t.Fatalf("workflow metrics queries after unchanged tick = %d, want %d", got, queries)
			}
			return
		case <-deadline:
			t.Fatal("unchanged snapshot did not reach the pre-enrichment skip path")
		}
	}
}

func (s *workflowSSECountingStore) WorkflowHistoryRevision(context.Context) (int64, error) {
	return s.revision.Load(), nil
}

func TestWorkflowHistorySharedAcrossSSEClients(t *testing.T) {
	t.Parallel()
	backend := &workflowSSECountingStore{}
	deps := testDeps(t)
	deps.Store = backend
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	server, err := web.NewServer(web.Config{SSETickInterval: time.Hour, SSEFragmentInterval: -1, Now: func() time.Time { return now }}, deps)
	if err != nil {
		t.Fatal(err)
	}
	addr := startWebServer(t, server)
	type client struct {
		conn   net.Conn
		reader *bufio.Reader
	}
	clients := make([]client, 3)
	for i := range clients {
		clients[i].conn, clients[i].reader = openRawEventStream(t, addr)
	}
	for _, tt := range []struct {
		name, title string
		seq         uint64
		elapsed     time.Duration
		correct     bool
		want        int64
	}{
		{name: "initial", title: "Initial operational row", seq: 1, want: 6},
		{name: "heartbeat", title: "Updated operational row", seq: 2, elapsed: time.Second, want: 6},
		{name: "correction", title: "Corrected history row", seq: 3, elapsed: 2 * time.Second, correct: true, want: 12},
		{name: "moving window", title: "Window refreshed row", seq: 4, elapsed: 32 * time.Second, want: 18},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.correct {
				backend.revision.Add(1)
			}
			snapshot := telemetry.Snapshot{Seq: tt.seq, GeneratedAt: now.Add(tt.elapsed), Running: []telemetry.Running{{Issue: telemetry.Issue{ID: "live", Identifier: "LIVE-1", Title: tt.title, State: "In Progress"}}}}
			if err := deps.Hub.Publish(snapshot); err != nil {
				t.Fatal(err)
			}
			for _, client := range clients {
				event := readRawSSEEventNamed(t, client.conn, client.reader, "snapshot")
				if !strings.Contains(event.data, tt.title) {
					t.Fatalf("operational row missing %q", tt.title)
				}
			}
			if got := backend.workflowMetricsCalls.Load(); got != tt.want {
				t.Fatalf("history queries = %d, want %d", got, tt.want)
			}
		})
	}
}
