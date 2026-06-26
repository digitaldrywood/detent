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
	sseEventSnapshot     = "snapshot"
	sseEventSidebar      = "sidebar"
	sseEventGitHubAPI    = "github-api-health"
	sseEventTick         = "tick"
	sseViewKanban        = "kanban"
	sseViewRuns          = "runs"
	sseViewDiagnostics   = "diagnostics"
	sseViewConfiguration = "configuration"
)

func staticSidebarNav(value string) string {
	switch strings.TrimSpace(value) {
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
			snapshot = s.cachedEnrichedSnapshot(ctx, snapshot)
			data := s.dashboardData(ctx, snapshot)
			if selectedProjectID != "" {
				if scopedData, ok := s.projectDashboardData(ctx, selectedProjectID, snapshot); ok {
					data = scopedData
				}
			}
			if selectedNav != "" {
				data.ActiveNav = selectedNav
			}
			snapshotComponent := templates.SnapshotView(data)
			if selectedView == sseViewKanban && (selectedProjectID == "" || data.ProjectID != "") {
				data.ActiveNav = "kanban"
				snapshotComponent = templates.ProjectKanbanSnapshot(data)
			} else if selectedView == sseViewRuns && selectedProjectID != "" {
				data.ActiveNav = "runs"
				snapshotComponent = templates.ProjectRunsSnapshot(data)
			} else if selectedView == sseViewDiagnostics && selectedProjectID != "" {
				data.ActiveNav = "diagnostics"
				snapshotComponent = templates.ProjectDiagnosticsSnapshot(data)
			} else if selectedView == sseViewConfiguration && selectedProjectID != "" {
				data.ActiveNav = "configuration"
			}
			sent, err := stream.sendComponent(ctx, res.Writer, sseEventSnapshot, snapshotComponent, s.sseFragmentInterval)
			if err != nil {
				return err
			}
			if ok, err := stream.sendComponent(ctx, res.Writer, sseEventSidebar, templates.DashboardSidebarContent(templates.DashboardShellDataFromDashboard(data)), s.sseFragmentInterval); err != nil {
				return err
			} else if ok {
				sent = true
			}
			if ok, err := stream.sendComponent(ctx, res.Writer, sseEventGitHubAPI, templates.GitHubAPIHealthChrome(data.Snapshot), s.sseHealthInterval); err != nil {
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

type sseStream struct {
	logger            *slog.Logger
	now               func() time.Time
	metricsEvery      time.Duration
	startedAt         time.Time
	nextMetricsAt     time.Time
	last              map[string]sseSentEvent
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
	rendered         int64
	renderedBytes    int64
	renderDuration   time.Duration
	sent             int64
	sentBytes        int64
	skippedUnchanged int64
	coalesced        int64
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
		pending:           make(map[string]ssePendingEvent),
		metrics:           make(map[string]*sseEventMetrics),
		pendingFlushOrder: []string{sseEventSnapshot, sseEventSidebar, sseEventGitHubAPI},
	}
}

func (s *sseStream) sendComponent(ctx context.Context, w io.Writer, name string, component templ.Component, minInterval time.Duration) (bool, error) {
	event, err := renderSSEComponent(ctx, name, component)
	if err != nil {
		return false, err
	}
	return s.sendRendered(ctx, w, event, minInterval)
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
		s.logger.Info("dashboard sse stream metrics",
			"event", name,
			"elapsed_seconds", elapsed,
			"rendered", metrics.rendered,
			"sent", metrics.sent,
			"sent_per_second", float64(metrics.sent)/elapsed,
			"rendered_payload_bytes", metrics.renderedBytes,
			"sent_payload_bytes", metrics.sentBytes,
			"render_duration", metrics.renderDuration,
			"skipped_unchanged", metrics.skippedUnchanged,
			"coalesced", metrics.coalesced,
			"pending", s.pendingEventQueued(name),
		)
	}
	s.nextMetricsAt = now.Add(s.metricsEvery)
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
