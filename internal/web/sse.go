package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	sseEventBuild        = "build"
	sseEventSnapshot     = "snapshot"
	sseEventGitHubAPI    = "github-api-health"
	sseEventTick         = "tick"
	sseEventLiveStatus   = "live-status"
	sseEventSidebarV2    = "sidebar-v2"
	sseViewHealth        = "health"
	sseViewAnalytics     = "analytics"
	sseViewBoard         = "board"
	sseViewFleet         = "fleet"
	sseViewOverview      = "overview"
	sseViewKanban        = "kanban"
	sseViewRuns          = "runs"
	sseViewDiagnostics   = "diagnostics"
	sseViewConfiguration = "configuration"
)

type sseSnapshotView struct {
	nav            string
	requireProject bool
	requireGlobal  bool
	kanbanFeedback bool
	component      func(templates.DashboardData) templ.Component
}

var sseSnapshotViews = map[string]sseSnapshotView{
	sseViewBoard: {
		nav:            "board",
		kanbanFeedback: true,
		component:      templates.BoardSnapshot,
	},
	sseViewFleet: {
		nav:           "fleet",
		requireGlobal: true,
		component:     templates.FleetSnapshotV2,
	},
	sseViewKanban: {
		nav:            "kanban",
		kanbanFeedback: true,
		component:      templates.BoardSnapshot,
	},
	sseViewOverview: {
		nav:            "overview",
		requireProject: true,
		component:      templates.ProjectOverviewSnapshotV2,
	},
	sseViewRuns: {
		nav:            "runs",
		requireProject: true,
		component:      templates.ProjectRunsSnapshotV2,
	},
	sseViewDiagnostics: {
		nav:       "diagnostics",
		component: templates.ProjectDiagnosticsSnapshot,
	},
	sseViewConfiguration: {
		nav:            "configuration",
		requireProject: true,
	},
}

func staticSidebarNav(value string) string {
	switch strings.TrimSpace(value) {
	case "health":
		return "health"
	case "analytics":
		return "analytics"
	case "reports":
		return "reports"
	case "settings":
		return "settings"
	default:
		return ""
	}
}

func (s *Server) events(c echo.Context) error {
	if scenario, ok, err := s.demoScenarioOrError(c); err != nil {
		return err
	} else if ok {
		return s.demoEvents(c, scenario)
	}
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming unsupported")
	}

	ctx := c.Request().Context()
	selectedProjectID := strings.TrimSpace(c.QueryParam("project"))
	selectedNav := staticSidebarNav(c.QueryParam("nav"))
	selectedView := strings.ToLower(strings.TrimSpace(c.QueryParam("view")))
	analyticsKind := c.QueryParam("kind")
	sub, err := s.hub.Subscribe(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return echo.NewHTTPError(http.StatusServiceUnavailable, "event hub unavailable").SetInternal(err)
	}
	defer sub.Close()
	stream := newSSEStream(s.logger, s.sseMetricsInterval)

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	res.Header().Set(echo.HeaderCacheControl, "no-cache")
	res.Header().Set("Connection", "keep-alive")
	res.Header().Set("X-Accel-Buffering", "no")
	res.WriteHeader(http.StatusOK)
	if err := writeSSEComponent(ctx, res.Writer, sseEventBuild, templates.LiveBuildVersion(s.version)); err != nil {
		return err
	}
	flusher.Flush()

	ticker := time.NewTicker(s.tickEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case snapshot, ok := <-sub.C():
			if !ok {
				return nil
			}
			snapshot = s.withManualRefresh(s.cachedEnrichedSnapshot(ctx, snapshot)).WithFreshness(s.now())
			data := s.dashboardData(ctx, snapshot)
			if selectedProjectID != "" {
				if scopedData, ok := s.projectDashboardData(ctx, selectedProjectID, snapshot); ok {
					data = scopedData
				}
			}
			data, snapshotComponent := s.sseSnapshotComponent(analyticsKind, selectedNav, selectedView, selectedProjectID, data)
			sent, err := stream.sendComponent(ctx, res.Writer, sseEventSnapshot, snapshotComponent, s.sseFragmentInterval)
			if err != nil {
				return err
			}
			shellData := templates.DashboardShellDataFromDashboard(data)
			if ok, err := stream.sendComponent(ctx, res.Writer, sseEventLiveStatus, templates.LiveStatus(shellData), s.sseFragmentInterval); err != nil {
				return err
			} else if ok {
				sent = true
			}
			healthFingerprint, err := templates.GitHubAPIHealthSidebarFingerprint(shellData)
			if err != nil {
				return err
			}
			if ok, err := stream.sendFingerprintedComponent(ctx, res.Writer, sseEventGitHubAPI, healthFingerprint, templates.GitHubAPIHealthSidebarItem(shellData), s.sseHealthInterval); err != nil {
				return err
			} else if ok {
				sent = true
			}
			sidebarFingerprint, err := templates.AppSidebarFingerprint(shellData)
			if err != nil {
				return err
			}
			if ok, err := stream.sendFingerprintedComponent(ctx, res.Writer, sseEventSidebarV2, sidebarFingerprint, templates.AppSidebarContent(shellData), s.sseFragmentInterval); err != nil {
				return err
			} else if ok {
				sent = true
			}
			if sent {
				flusher.Flush()
			}
		case now := <-ticker.C:
			sent, err := stream.flushPending(ctx, res.Writer)
			if err != nil {
				return err
			}
			if ok, err := stream.sendComponent(ctx, res.Writer, sseEventTick, templates.LiveTick(now), 0); err != nil {
				return err
			} else if ok {
				sent = true
			}
			if sent {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) sseSnapshotComponent(
	analyticsKind string,
	selectedNav string,
	selectedView string,
	selectedProjectID string,
	data templates.DashboardData,
) (templates.DashboardData, templ.Component) {
	if selectedNav != "" {
		data.ActiveNav = selectedNav
	}
	switch selectedNav {
	case sseViewHealth:
		return data, templates.HealthSnapshotV2(data)
	case sseViewAnalytics:
		data.AnalyticsKind = strings.TrimSpace(analyticsKind)
		return data, templates.AnalyticsSnapshotV2(data)
	}

	view, ok := sseSnapshotViews[selectedView]
	if !ok || !view.available(selectedProjectID, data) {
		return data, templates.SnapshotView(data)
	}
	if view.nav != "" {
		data.ActiveNav = view.nav
	}
	if view.kanbanFeedback {
		data = s.withKanbanRefreshFeedback(data)
	}
	if view.component == nil {
		return data, templates.SnapshotView(data)
	}
	return data, view.component(data)
}

func (view sseSnapshotView) available(selectedProjectID string, data templates.DashboardData) bool {
	switch {
	case view.requireGlobal && selectedProjectID != "":
		return false
	case view.requireProject && selectedProjectID == "":
		return false
	case view.kanbanFeedback && selectedProjectID != "" && data.ProjectID == "":
		return false
	default:
		return true
	}
}

type sseStream struct {
	logger            *slog.Logger
	now               func() time.Time
	metricsEvery      time.Duration
	startedAt         time.Time
	nextMetricsAt     time.Time
	last              map[string]sseSentEvent
	lastFingerprint   map[string]templates.SSEFingerprint
	pending           map[string]ssePendingEvent
	metrics           map[string]*sseEventMetrics
	pendingFlushOrder []string
}

type sseRenderedEvent struct {
	name           string
	body           string
	payloadBytes   int
	renderDuration time.Duration
}

type sseSentEvent struct {
	body   string
	sentAt time.Time
}

type ssePendingEvent struct {
	body         string
	payloadBytes int
	minInterval  time.Duration
}

type sseEventMetrics struct {
	rendered           int64
	renderedBytes      int64
	renderDuration     time.Duration
	sent               int64
	sentBytes          int64
	skippedUnchanged   int64
	skippedFingerprint int64
	coalesced          int64
}

func newSSEStream(logger *slog.Logger, metricsEvery time.Duration) *sseStream {
	now := time.Now().UTC()
	if logger == nil {
		logger = slog.Default()
	}
	return &sseStream{
		logger:            logger,
		now:               func() time.Time { return time.Now().UTC() },
		metricsEvery:      metricsEvery,
		startedAt:         now,
		nextMetricsAt:     now.Add(metricsEvery),
		last:              make(map[string]sseSentEvent),
		lastFingerprint:   make(map[string]templates.SSEFingerprint),
		pending:           make(map[string]ssePendingEvent),
		metrics:           make(map[string]*sseEventMetrics),
		pendingFlushOrder: []string{sseEventSnapshot, sseEventLiveStatus, sseEventGitHubAPI, sseEventSidebarV2},
	}
}

func (s *sseStream) sendComponent(ctx context.Context, w io.Writer, name string, component templ.Component, minInterval time.Duration) (bool, error) {
	event, err := renderSSEComponent(ctx, name, component)
	if err != nil {
		return false, err
	}
	return s.sendRendered(ctx, w, event, minInterval)
}

func (s *sseStream) sendFingerprintedComponent(ctx context.Context, w io.Writer, name string, fingerprint templates.SSEFingerprint, component templ.Component, minInterval time.Duration) (bool, error) {
	if last, ok := s.lastFingerprint[name]; ok && fingerprint == last {
		now := s.currentTime()
		s.metricsFor(name).skippedFingerprint++
		s.logMetricsIfDue(now)
		return false, nil
	}
	event, err := renderSSEComponent(ctx, name, component)
	if err != nil {
		return false, err
	}
	sent, err := s.sendRendered(ctx, w, event, minInterval)
	if err != nil {
		return false, err
	}
	s.lastFingerprint[name] = fingerprint
	return sent, nil
}

func (s *sseStream) sendRendered(ctx context.Context, w io.Writer, event sseRenderedEvent, minInterval time.Duration) (bool, error) {
	now := s.currentTime()
	metrics := s.metricsFor(event.name)
	metrics.rendered++
	metrics.renderedBytes += int64(event.payloadBytes)
	metrics.renderDuration += event.renderDuration

	if last, ok := s.last[event.name]; ok && event.body == last.body {
		delete(s.pending, event.name)
		metrics.skippedUnchanged++
		s.logMetricsIfDue(now)
		return false, nil
	}
	if last, ok := s.last[event.name]; ok && minInterval > 0 && now.Sub(last.sentAt) < minInterval {
		s.pending[event.name] = ssePendingEvent{
			body:         event.body,
			payloadBytes: event.payloadBytes,
			minInterval:  minInterval,
		}
		metrics.coalesced++
		s.logMetricsIfDue(now)
		return false, nil
	}

	if err := s.writeFrame(ctx, w, event.name, event.body, event.payloadBytes, now); err != nil {
		return false, err
	}
	s.logMetricsIfDue(now)
	return true, nil
}

func (s *sseStream) flushPending(ctx context.Context, w io.Writer) (bool, error) {
	now := s.currentTime()
	sent := false
	for _, name := range s.pendingFlushOrder {
		event, ok := s.pending[name]
		if !ok {
			continue
		}
		if last, ok := s.last[name]; ok && event.minInterval > 0 && now.Sub(last.sentAt) < event.minInterval {
			continue
		}
		if last, ok := s.last[name]; ok && event.body == last.body {
			delete(s.pending, name)
			s.metricsFor(name).skippedUnchanged++
			continue
		}
		if err := s.writeFrame(ctx, w, name, event.body, event.payloadBytes, now); err != nil {
			return sent, err
		}
		delete(s.pending, name)
		sent = true
	}
	s.logMetricsIfDue(now)
	return sent, nil
}

func (s *sseStream) writeFrame(ctx context.Context, w io.Writer, name string, body string, payloadBytes int, now time.Time) error {
	if err := writeSSEFrame(w, name, body); err != nil {
		return err
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.last[name] = sseSentEvent{body: body, sentAt: now}
	metrics := s.metricsFor(name)
	metrics.sent++
	metrics.sentBytes += int64(payloadBytes)
	return nil
}

func (s *sseStream) metricsFor(name string) *sseEventMetrics {
	metrics := s.metrics[name]
	if metrics == nil {
		metrics = &sseEventMetrics{}
		s.metrics[name] = metrics
	}
	return metrics
}

func (s *sseStream) currentTime() time.Time {
	if s == nil || s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *sseStream) logMetricsIfDue(now time.Time) {
	if s == nil || s.metricsEvery <= 0 || s.logger == nil || now.Before(s.nextMetricsAt) {
		return
	}
	elapsed := now.Sub(s.startedAt).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	for name, metrics := range s.metrics {
		s.logger.Debug("dashboard sse stream metrics",
			"event", name,
			"elapsed_seconds", elapsed,
			"rendered", metrics.rendered,
			"sent", metrics.sent,
			"sent_per_second", float64(metrics.sent)/elapsed,
			"rendered_payload_bytes", metrics.renderedBytes,
			"sent_payload_bytes", metrics.sentBytes,
			"render_duration", metrics.renderDuration,
			"skipped_unchanged", metrics.skippedUnchanged,
			"skipped_fingerprint", metrics.skippedFingerprint,
			"discard_ratio", metrics.discardRatio(),
			"coalesced", metrics.coalesced,
			"pending", s.pendingEventQueued(name),
		)
	}
	s.nextMetricsAt = now.Add(s.metricsEvery)
}

func (m *sseEventMetrics) discardRatio() float64 {
	if m == nil || m.rendered == 0 {
		return 0
	}
	discarded := m.rendered - m.sent
	if discarded <= 0 {
		return 0
	}
	return float64(discarded) / float64(m.rendered)
}

func (s *sseStream) pendingEventQueued(name string) bool {
	_, ok := s.pending[name]
	return ok
}

func writeSSEComponent(ctx context.Context, w io.Writer, event string, component templ.Component) error {
	rendered, err := renderSSEComponent(ctx, event, component)
	if err != nil {
		return err
	}
	return writeSSEFrame(w, rendered.name, rendered.body)
}

func renderSSEComponent(ctx context.Context, event string, component templ.Component) (sseRenderedEvent, error) {
	var body bytes.Buffer
	started := time.Now()
	if err := component.Render(ctx, &body); err != nil {
		return sseRenderedEvent{}, err
	}
	renderedBody := body.String()
	return sseRenderedEvent{
		name:           event,
		body:           renderedBody,
		payloadBytes:   len(renderedBody),
		renderDuration: time.Since(started),
	}, nil
}

func writeSSEFrame(w io.Writer, event string, body string) error {
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
