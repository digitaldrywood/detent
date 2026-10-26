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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

const issueExplanationTimeout = 5 * time.Second

type IssueExplainer interface {
	Explain(context.Context, explain.Query) (explain.IssueExplanation, error)
}

func (s *Server) apiIssueExplanation(c echo.Context) error {
	ctx, err := operatortool.AuthorizeCurrent(c.Request().Context(), operatortool.Requirement{Scope: apikey.ScopeRead, ProjectID: c.Param("project_id")})
	if err != nil {
		return writeAPIAuthError(c, http.StatusForbidden, "access_denied", operatortool.ErrAccessDenied.Error())
	}
	c.SetRequest(c.Request().WithContext(ctx))
	explanation, ok, err := s.issueExplanation(c)
	if !ok {
		return err
	}
	return c.JSON(http.StatusOK, explanation)
}

func (s *Server) issueExplanation(c echo.Context) (explain.IssueExplanation, bool, error) {
	projectID := strings.TrimSpace(c.Param("project_id"))
	reference := strings.TrimSpace(c.QueryParam("reference"))
	if projectID == "" || reference == "" {
		return explain.IssueExplanation{}, false, c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and reference are required"))
	}
	if response, ok := issueExplanationVersionProblem(c.QueryParam("schema")); ok {
		return explain.IssueExplanation{}, false, c.JSON(response.status, errorResponse(response.code, response.message))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), issueExplanationTimeout)
	defer cancel()
	explanation, err := (nativeIssueExplainer{server: s, fallback: s.issueExplainer}).Explain(ctx, explain.Query{ProjectID: projectID, Reference: reference})
	if err == nil {
		return explanation, true, nil
	}

	var ambiguous *explain.AmbiguousIdentityError
	switch {
	case errors.Is(err, explain.ErrProjectRequired), errors.Is(err, explain.ErrIssueReferenceNeeded):
		return explain.IssueExplanation{}, false, c.JSON(http.StatusBadRequest, errorResponse("bad_request", "project_id and reference are required"))
	case errors.As(err, &ambiguous):
		return explain.IssueExplanation{}, false, c.JSON(http.StatusConflict, errorResponse("ambiguous_reference", "Issue reference is ambiguous"))
	case errors.Is(err, explain.ErrNotFound):
		return explain.IssueExplanation{}, false, c.JSON(http.StatusNotFound, errorResponse("issue_not_found", "Issue not found"))
	default:
		s.logger.Error("issue explanation failed", slog.Any("error", err))
		return explain.IssueExplanation{}, false, c.JSON(http.StatusServiceUnavailable, errorResponse("runtime_unavailable", "Issue explanation runtime is unavailable"))
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
