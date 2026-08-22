package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	kanbanstate "github.com/digitaldrywood/detent/internal/kanban"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestSSESnapshotComponent(t *testing.T) {
	t.Parallel()

	type snapshotCase struct {
		name              string
		analyticsKind     string
		selectedNav       string
		selectedView      string
		selectedProjectID string
		dataProjectID     string
		wantActiveNav     string
		wantAnalyticsKind string
		wantData          func(*Server, templates.DashboardData) templates.DashboardData
		wantComponent     func(templates.DashboardData) templ.Component
	}

	cases := []snapshotCase{
		{
			name:              "health nav wins before selected view",
			selectedNav:       sseViewHealth,
			selectedView:      sseViewBoard,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewHealth,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewHealth
				return data
			},
			wantComponent: templates.HealthSnapshotV2,
		},
		{
			name:              "analytics nav trims kind and wins before selected view",
			analyticsKind:     " attempts ",
			selectedNav:       sseViewAnalytics,
			selectedView:      sseViewFleet,
			wantActiveNav:     sseViewAnalytics,
			wantAnalyticsKind: "attempts",
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewAnalytics
				data.AnalyticsKind = "attempts"
				return data
			},
			wantComponent: templates.AnalyticsSnapshotV2,
		},
		{
			name:          "other nav keeps default snapshot",
			selectedNav:   "reports",
			wantActiveNav: "reports",
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = "reports"
				return data
			},
			wantComponent: templates.SnapshotView,
		},
		{
			name:          "board global view uses board snapshot",
			selectedView:  sseViewBoard,
			wantActiveNav: sseViewBoard,
			wantData: func(server *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewBoard
				return server.withKanbanRefreshFeedback(data)
			},
			wantComponent: templates.BoardSnapshot,
		},
		{
			name:              "board project view uses board snapshot with scoped data",
			selectedView:      sseViewBoard,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewBoard,
			wantData: func(server *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewBoard
				return server.withKanbanRefreshFeedback(data)
			},
			wantComponent: templates.BoardSnapshot,
		},
		{
			name:              "board project view falls through without scoped data",
			selectedView:      sseViewBoard,
			selectedProjectID: "missing",
			wantActiveNav:     "initial",
			wantComponent:     templates.SnapshotView,
		},
		{
			name:          "fleet global view uses fleet snapshot",
			selectedView:  sseViewFleet,
			wantActiveNav: sseViewFleet,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewFleet
				return data
			},
			wantComponent: templates.FleetSnapshotV2,
		},
		{
			name:              "fleet project view falls through",
			selectedView:      sseViewFleet,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     "initial",
			wantComponent:     templates.SnapshotView,
		},
		{
			name:          "kanban global view uses board snapshot",
			selectedView:  sseViewKanban,
			wantActiveNav: sseViewKanban,
			wantData: func(server *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewKanban
				return server.withKanbanRefreshFeedback(data)
			},
			wantComponent: templates.BoardSnapshot,
		},
		{
			name:              "kanban project view uses board snapshot with scoped data",
			selectedView:      sseViewKanban,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewKanban,
			wantData: func(server *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewKanban
				return server.withKanbanRefreshFeedback(data)
			},
			wantComponent: templates.BoardSnapshot,
		},
		{
			name:              "kanban project view falls through without scoped data",
			selectedView:      sseViewKanban,
			selectedProjectID: "missing",
			wantActiveNav:     "initial",
			wantComponent:     templates.SnapshotView,
		},
		{
			name:              "overview project view uses overview snapshot",
			selectedView:      sseViewOverview,
			selectedProjectID: "detent",
			wantActiveNav:     sseViewOverview,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewOverview
				return data
			},
			wantComponent: templates.ProjectOverviewSnapshotV2,
		},
		{
			name:          "overview global view falls through",
			selectedView:  sseViewOverview,
			wantActiveNav: "initial",
			wantComponent: templates.SnapshotView,
		},
		{
			name:              "runs project view uses runs snapshot",
			selectedView:      sseViewRuns,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewRuns,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewRuns
				return data
			},
			wantComponent: templates.ProjectRunsSnapshotV2,
		},
		{
			name:          "runs global view falls through",
			selectedView:  sseViewRuns,
			wantActiveNav: "initial",
			wantComponent: templates.SnapshotView,
		},
		{
			name:              "diagnostics project view uses diagnostics snapshot",
			selectedView:      sseViewDiagnostics,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewDiagnostics,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewDiagnostics
				return data
			},
			wantComponent: templates.ProjectDiagnosticsSnapshot,
		},
		{
			name:          "diagnostics global view uses fleet diagnostics snapshot",
			selectedView:  sseViewDiagnostics,
			wantActiveNav: sseViewDiagnostics,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewDiagnostics
				return data
			},
			wantComponent: templates.ProjectDiagnosticsSnapshot,
		},
		{
			name:              "configuration project view keeps default snapshot",
			selectedView:      sseViewConfiguration,
			selectedProjectID: "detent",
			dataProjectID:     "detent",
			wantActiveNav:     sseViewConfiguration,
			wantData: func(_ *Server, data templates.DashboardData) templates.DashboardData {
				data.ActiveNav = sseViewConfiguration
				return data
			},
			wantComponent: templates.SnapshotView,
		},
		{
			name:          "configuration global view falls through",
			selectedView:  sseViewConfiguration,
			wantActiveNav: "initial",
			wantComponent: templates.SnapshotView,
		},
	}

	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := testSSESnapshotData(tt.dataProjectID)
			server := newSSESnapshotTestServer()
			gotData, gotComponent := server.sseSnapshotComponent(tt.analyticsKind, tt.selectedNav, tt.selectedView, tt.selectedProjectID, data)

			wantData := testSSESnapshotData(tt.dataProjectID)
			if tt.wantData != nil {
				wantData = tt.wantData(newSSESnapshotTestServer(), wantData)
			}
			gotHTML := renderSnapshotComponent(t, gotComponent)
			wantHTML := renderSnapshotComponent(t, tt.wantComponent(wantData))

			if gotData.ActiveNav != tt.wantActiveNav {
				t.Fatalf("ActiveNav = %q, want %q", gotData.ActiveNav, tt.wantActiveNav)
			}
			if gotData.AnalyticsKind != tt.wantAnalyticsKind {
				t.Fatalf("AnalyticsKind = %q, want %q", gotData.AnalyticsKind, tt.wantAnalyticsKind)
			}
			if gotHTML != wantHTML {
				t.Fatalf("rendered component mismatch\n got:\n%s\nwant:\n%s", gotHTML, wantHTML)
			}
		})
	}
}

func TestSSEStreamSkipsUnchangedFragments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newTestSSEStream(&now)
	var out bytes.Buffer

	sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "<section>same</section>",
		payloadBytes: len("<section>same</section>"),
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("sendRendered() error = %v", err)
	}
	if !sent {
		t.Fatal("first sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	sent, err = stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "<section>same</section>",
		payloadBytes: len("<section>same</section>"),
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("sendRendered() unchanged error = %v", err)
	}
	if sent {
		t.Fatal("unchanged sendRendered() sent = true, want false")
	}
	if got := strings.Count(out.String(), "event: snapshot"); got != 1 {
		t.Fatalf("snapshot event count = %d, want 1; output:\n%s", got, out.String())
	}
	metrics := stream.metricsFor(sseEventSnapshot)
	if metrics.skippedUnchanged != 1 {
		t.Fatalf("skippedUnchanged = %d, want 1", metrics.skippedUnchanged)
	}
}

func TestSSEStreamSkipsMatchingFingerprintBeforeRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		fingerprint            templates.SSEFingerprint
		secondBody             string
		wantRenders            int
		wantSent               int64
		wantSkippedFingerprint int64
		wantSkippedUnchanged   int64
		wantDiscardRatio       float64
	}{
		{
			name:                   "matching fingerprint bypasses renderer",
			fingerprint:            templates.SSEFingerprint{1},
			secondBody:             "changed but unreachable",
			wantRenders:            1,
			wantSent:               1,
			wantSkippedFingerprint: 1,
		},
		{
			name:                 "changed fingerprint renders identical output",
			fingerprint:          templates.SSEFingerprint{2},
			secondBody:           "initial",
			wantRenders:          2,
			wantSent:             1,
			wantSkippedUnchanged: 1,
			wantDiscardRatio:     0.5,
		},
		{
			name:        "changed fingerprint renders changed output",
			fingerprint: templates.SSEFingerprint{2},
			secondBody:  "changed",
			wantRenders: 2,
			wantSent:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			stream := newTestSSEStream(&now)
			var out bytes.Buffer
			renders := 0
			component := func(body string) templ.Component {
				return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
					renders++
					_, err := io.WriteString(w, body)
					return err
				})
			}

			if sent, err := stream.sendFingerprintedComponent(context.Background(), &out, sseEventSidebarV2, templates.SSEFingerprint{1}, component("initial"), 0); err != nil {
				t.Fatalf("first sendFingerprintedComponent() error = %v", err)
			} else if !sent {
				t.Fatal("first sendFingerprintedComponent() sent = false, want true")
			}
			if _, err := stream.sendFingerprintedComponent(context.Background(), &out, sseEventSidebarV2, tt.fingerprint, component(tt.secondBody), 0); err != nil {
				t.Fatalf("second sendFingerprintedComponent() error = %v", err)
			}

			metrics := stream.metricsFor(sseEventSidebarV2)
			if renders != tt.wantRenders {
				t.Fatalf("render calls = %d, want %d", renders, tt.wantRenders)
			}
			if metrics.sent != tt.wantSent {
				t.Fatalf("sent = %d, want %d", metrics.sent, tt.wantSent)
			}
			if metrics.skippedFingerprint != tt.wantSkippedFingerprint {
				t.Fatalf("skippedFingerprint = %d, want %d", metrics.skippedFingerprint, tt.wantSkippedFingerprint)
			}
			if metrics.skippedUnchanged != tt.wantSkippedUnchanged {
				t.Fatalf("skippedUnchanged = %d, want %d", metrics.skippedUnchanged, tt.wantSkippedUnchanged)
			}
			if got := metrics.discardRatio(); got != tt.wantDiscardRatio {
				t.Fatalf("discardRatio() = %v, want %v", got, tt.wantDiscardRatio)
			}
		})
	}
}

func TestSSEStreamCoalescesPendingFragments(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newTestSSEStream(&now)
	var out bytes.Buffer

	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "initial",
		payloadBytes: len("initial"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() initial error = %v", err)
	} else if !sent {
		t.Fatal("initial sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "stale",
		payloadBytes: len("stale"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() stale error = %v", err)
	} else if sent {
		t.Fatal("stale sendRendered() sent = true, want coalesced")
	}

	now = now.Add(time.Second)
	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "latest",
		payloadBytes: len("latest"),
	}, 5*time.Second); err != nil {
		t.Fatalf("sendRendered() latest error = %v", err)
	} else if sent {
		t.Fatal("latest sendRendered() sent = true, want coalesced")
	}

	now = now.Add(3 * time.Second)
	if sent, err := stream.flushPending(context.Background(), &out); err != nil {
		t.Fatalf("flushPending() error = %v", err)
	} else if !sent {
		t.Fatal("flushPending() sent = false, want true")
	}

	output := out.String()
	if !strings.Contains(output, "data: latest") {
		t.Fatalf("coalesced output missing latest body:\n%s", output)
	}
	if strings.Contains(output, "data: stale") {
		t.Fatalf("coalesced output sent stale body:\n%s", output)
	}
	if got := strings.Count(output, "event: snapshot"); got != 2 {
		t.Fatalf("snapshot event count = %d, want 2; output:\n%s", got, output)
	}
	metrics := stream.metricsFor(sseEventSnapshot)
	if metrics.coalesced != 2 {
		t.Fatalf("coalesced = %d, want 2", metrics.coalesced)
	}
	if metrics.sent != 2 {
		t.Fatalf("sent = %d, want 2", metrics.sent)
	}
	if got, want := metrics.discardRatio(), float64(1)/3; got != want {
		t.Fatalf("discardRatio() = %v, want %v", got, want)
	}
}

func TestSSEStreamLogsMetricsByEvent(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	stream := newSSEStream(logger, time.Second)
	stream.startedAt = now
	stream.nextMetricsAt = now.Add(time.Second)
	stream.now = func() time.Time { return now }
	var out bytes.Buffer

	if sent, err := stream.sendRendered(context.Background(), &out, sseRenderedEvent{
		name:         sseEventSnapshot,
		body:         "payload",
		payloadBytes: len("payload"),
	}, 0); err != nil {
		t.Fatalf("sendRendered() error = %v", err)
	} else if !sent {
		t.Fatal("sendRendered() sent = false, want true")
	}

	now = now.Add(time.Second)
	stream.logMetricsIfDue(now)

	for _, want := range []string{
		"dashboard sse stream metrics",
		"event=snapshot",
		"sent_per_second",
		"sent_payload_bytes",
		"rendered_payload_bytes",
		"skipped_fingerprint=0",
		"discard_ratio=0",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("metrics log missing %q:\n%s", want, logs.String())
		}
	}
}

func newTestSSEStream(now *time.Time) *sseStream {
	stream := newSSEStream(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour)
	stream.startedAt = now.UTC()
	stream.nextMetricsAt = now.Add(time.Hour)
	stream.now = func() time.Time { return now.UTC() }
	return stream
}

func newSSESnapshotTestServer() *Server {
	return &Server{
		kanbanMutations: kanbanstate.NewMutationTracker(),
		kanbanRefreshes: newKanbanRefreshFeedbackTracker(),
	}
}

func testSSESnapshotData(projectID string) templates.DashboardData {
	generatedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	lastRefreshAt := generatedAt.Add(-time.Minute)
	data := templates.DashboardData{
		Title:           "Detent",
		ApplicationName: "Detent",
		InstanceName:    "dogfood",
		ConnectorName:   "github",
		DashboardURL:    "http://127.0.0.1:0",
		ActiveNav:       "initial",
		ProjectID:       strings.TrimSpace(projectID),
		ProjectName:     "Detent",
		Kanban: templates.KanbanData{
			ProjectID: projectID,
			States:    []string{"Todo", "In Progress", "Done"},
		},
		Snapshot: telemetry.Snapshot{
			GeneratedAt: generatedAt,
			Project: telemetry.Project{
				ID:          projectID,
				DisplayName: "Detent",
				URL:         "https://github.com/digitaldrywood/detent/issues",
			},
			Instance: telemetry.Instance{
				Name:                    "dogfood",
				GitHubLogin:             "detent-bot",
				AuthorizationScope:      "repo",
				AuthorizationConfigured: true,
			},
			Refresh: telemetry.Refresh{
				Status:        telemetry.RefreshStatusReady,
				LastRefreshAt: &lastRefreshAt,
				DataSeq:       1,
			},
			Counts: telemetry.Counts{
				Running:   1,
				Queue:     2,
				Blocked:   0,
				Completed: 3,
			},
			BoardIssues: []telemetry.Issue{
				{
					ID:         "issue-1018",
					Identifier: "DDW-1018",
					ProjectID:  projectID,
					Title:      "Refactor SSE snapshot dispatch",
					State:      "In Progress",
				},
			},
		},
	}
	if projectID != "" {
		data.Snapshot.Projects = []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{
					ID:          projectID,
					DisplayName: "Detent",
					URL:         "https://github.com/digitaldrywood/detent/issues",
				},
				Counts: data.Snapshot.Counts,
				Refresh: telemetry.Refresh{
					Status:        telemetry.RefreshStatusReady,
					LastRefreshAt: &lastRefreshAt,
					DataSeq:       1,
				},
			},
		}
	}
	return data
}

func renderSnapshotComponent(t *testing.T, component templ.Component) string {
	t.Helper()

	var buf bytes.Buffer
	if err := component.Render(context.Background(), &buf); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return buf.String()
}
