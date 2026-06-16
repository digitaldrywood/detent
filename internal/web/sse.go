package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/web/templates"
)

const (
	sseEventSnapshot = "snapshot"
	sseEventSidebar  = "sidebar"
	sseEventTick     = "tick"
	sseViewKanban    = "kanban"
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
			} else if selectedNav != "" {
				data.ActiveNav = selectedNav
			}
			snapshotComponent := templates.SnapshotView(data)
			if selectedView == sseViewKanban && selectedProjectID != "" {
				snapshotComponent = templates.ProjectKanbanSnapshot(data)
			}
			if err := writeSSEComponent(ctx, res.Writer, sseEventSnapshot, snapshotComponent); err != nil {
				return err
			}
			if selectedView != sseViewKanban {
				if err := writeSSEComponent(ctx, res.Writer, sseEventSidebar, templates.DashboardSidebarContent(templates.DashboardShellDataFromDashboard(data))); err != nil {
					return err
				}
			}
			flusher.Flush()
		case now := <-ticker.C:
			if err := writeSSEComponent(ctx, res.Writer, sseEventTick, templates.LiveTick(now)); err != nil {
				return err
			}
			flusher.Flush()
		}
	}
}

func writeSSEComponent(ctx context.Context, w io.Writer, event string, component templ.Component) error {
	var body bytes.Buffer
	if err := component.Render(ctx, &body); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(body.String(), "\n"), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
