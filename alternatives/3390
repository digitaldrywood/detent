package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const operatorToolTimeout = 5 * time.Second

func (s *Server) apiOperatorTool(c echo.Context) error {
	if s.operatorTools == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Operator read runtime is unavailable"))
	}

	body := http.MaxBytesReader(c.Response(), c.Request().Body, operatortool.MaxArgumentBytes)
	arguments, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return c.JSON(http.StatusRequestEntityTooLarge, errorResponse("arguments_too_large", "Operator tool arguments exceed the size limit"))
		}
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_arguments", "Operator tool arguments could not be read"))
	}
	if len(strings.TrimSpace(string(arguments))) == 0 {
		arguments = []byte(`{}`)
	}

	ctx, cancel := context.WithTimeout(s.chatContext(c), operatorToolTimeout)
	defer cancel()
	result, err := dashboardOperatorExecutor{server: s}.Execute(ctx, operatortool.Call{
		Name:      strings.TrimSpace(c.Param("tool_name")),
		Arguments: arguments,
	})
	if err == nil {
		return c.Blob(http.StatusOK, echo.MIMEApplicationJSON, result.Content)
	}

	var ambiguous *explain.AmbiguousIdentityError
	switch {
	case errors.Is(err, mutation.ErrConflict):
		return c.JSON(http.StatusConflict, errorResponse("idempotency_conflict", mutation.ErrConflict.Error()))
	case errors.Is(err, mutation.ErrUncertain):
		return c.JSON(http.StatusConflict, errorResponse("idempotency_in_progress", mutation.ErrUncertain.Error()))
	case errors.Is(err, operatortool.ErrAccessDenied):
		return c.JSON(http.StatusForbidden, errorResponse("access_denied", operatortool.ErrAccessDenied.Error()))
	case errors.Is(err, operatortool.ErrUnknownTool):
		return c.JSON(http.StatusNotFound, errorResponse("unknown_operator_tool", "Read-only operator tool not found"))
	case errors.Is(err, operatortool.ErrInvalidArguments), errors.Is(err, explain.ErrProjectRequired), errors.Is(err, explain.ErrIssueReferenceNeeded):
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_arguments", "Invalid operator tool arguments"))
	case errors.As(err, &ambiguous):
		return c.JSON(http.StatusConflict, errorResponse("ambiguous_reference", "Issue reference is ambiguous"))
	case errors.Is(err, explain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	case errors.Is(err, operatortool.ErrSnapshotUnavailable):
		return c.JSON(http.StatusServiceUnavailable, errorResponse("snapshot_unavailable", "Operator telemetry snapshot is unavailable"))
	default:
		s.logger.Error("operator tool unavailable")
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Operator read runtime is unavailable"))
	}
}

func (s *Server) apiOperatorTools(c echo.Context) error {
	definitions, err := dashboardOperatorExecutor{server: s}.ListTools(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusForbidden, errorResponse("access_denied", operatortool.ErrAccessDenied.Error()))
	}
	return c.JSON(http.StatusOK, struct {
		Tools []operatortool.Definition `json:"tools"`
	}{definitions})
}
