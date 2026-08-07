package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
)

const issueExplanationTimeout = 5 * time.Second

type IssueExplainer interface {
	Explain(context.Context, explain.Query) (explain.IssueExplanation, error)
}

func (s *Server) apiIssueExplanation(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	reference := strings.TrimSpace(c.QueryParam("reference"))
	if projectID == "" || reference == "" {
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and reference are required"))
	}
	if response, ok := issueExplanationVersionProblem(c.QueryParam("schema")); ok {
		return c.JSON(response.status, errorResponse(response.code, response.message))
	}
	if s.issueExplainer == nil {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Issue explanation runtime is unavailable"))
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), issueExplanationTimeout)
	defer cancel()
	explanation, err := s.issueExplainer.Explain(ctx, explain.Query{ProjectID: projectID, Reference: reference})
	if err == nil {
		return c.JSON(http.StatusOK, explanation)
	}

	var ambiguous *explain.AmbiguousIdentityError
	switch {
	case errors.Is(err, explain.ErrProjectRequired), errors.Is(err, explain.ErrIssueReferenceNeeded):
		return c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and reference are required"))
	case errors.As(err, &ambiguous):
		return c.JSON(http.StatusConflict, errorResponse("ambiguous_reference", "Issue reference is ambiguous"))
	case errors.Is(err, explain.ErrNotFound):
		return c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	default:
		s.logger.Error("issue explanation failed", slog.Any("error", err))
		return c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Issue explanation runtime is unavailable"))
	}
}

type issueExplanationProblem struct {
	status  int
	code    string
	message string
}

func issueExplanationVersionProblem(raw string) (issueExplanationProblem, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return issueExplanationProblem{}, false
	}
	version, err := strconv.Atoi(raw)
	if err != nil || version <= 0 {
		return issueExplanationProblem{status: http.StatusBadRequest, code: "bad_request", message: "schema must be a positive integer"}, true
	}
	if version != explain.SchemaVersion {
		return issueExplanationProblem{status: http.StatusConflict, code: "version_conflict", message: "Requested schema version is not supported"}, true
	}
	return issueExplanationProblem{}, false
}
