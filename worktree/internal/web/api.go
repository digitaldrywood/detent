package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/orchestrator"
)

type healthStateEndpoints struct {
	Fleet   healthStateEndpoint `json:"fleet"`
	Project healthStateEndpoint `json:"project"`
}

type healthStateEndpoint struct {
	Status               string `json:"status"`
	TargetLatencyMS      int64  `json:"target_latency_ms"`
	RequestCount         uint64 `json:"request_count"`
	InFlight             int64  `json:"in_flight"`
	LastLatencyMS        int64  `json:"last_latency_ms"`
	MaxLatencyMS         int64  `json:"max_latency_ms"`
	SlowRequestCount     uint64 `json:"slow_request_count"`
	TimeoutCount         uint64 `json:"timeout_count"`
	CanceledRequestCount uint64 `json:"canceled_request_count"`
	LastOutcome          string `json:"last_outcome"`
}

type Refresher interface {
	RequestRefresh(context.Context) (RefreshResponse, error)
}

type TargetedRefresher interface {
	RequestTargetedRefresh(context.Context, RefreshTarget) (RefreshResponse, error)
}

type RefreshTarget struct {
	Repository        string   `json:"repository,omitempty"`
	IssueNumber       int      `json:"issue_number,omitempty"`
	PullRequestNumber int      `json:"pull_request_number,omitempty"`
	SHA               string   `json:"sha,omitempty"`
	Branch            string   `json:"branch,omitempty"`
	Event             string   `json:"event,omitempty"`
	DeliveryID        string   `json:"delivery_id,omitempty"`
	ProjectIDs        []string `json:"-"`
}

type RefreshResponse = orchestrator.RefreshResponse

func (s *Server) handleHTTPError(err error, c echo.Context) {
	if c.Response().Committed {
		return
	}

	status := http.StatusInternalServerError
	payload := errorResponse("internal_server_error", "Internal server error")

	var httpErr *echo.HTTPError
	if errors.As(err, &httpErr) {
		status = httpErr.Code
		switch status {
		case http.StatusNotFound:
			payload = errorResponse("not_found", "Route not found")
		case http.StatusMethodNotAllowed:
			payload = errorResponse("method_not_allowed", "Method not allowed")
		}
	}

	if writeErr := c.JSON(status, payload); writeErr != nil {
		s.logger.Error("write http error response failed", slog.Any("error", writeErr))
	}
}

func errorResponse(code string, message string) apiErrorResponse {
	return apiErrorResponse{
		Error: apiError{
			Code:    code,
			Message: message,
		},
	}
}

type apiErrorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
