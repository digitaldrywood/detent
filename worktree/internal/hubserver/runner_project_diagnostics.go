package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e hubProjectExecutor) runnerProjectDiagnostics(ctx context.Context, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
	scope, err := e.projectScope(ctx)
	if err != nil {
		return operatortool.Result{}, err
	}
	page, err := e.service.readRunnerProjectDiagnostics(ctx, scope, args)
	if err != nil {
		return operatortool.Result{}, err
	}
	return hubProjectResult(page)
}

func (s *Service) getRunnerProjectDiagnostics(c echo.Context) error {
	scope := nativeRequestScope(c)
	args := operatortool.LocalProjectArguments{ProjectID: string(scope.project), RunnerID: c.QueryParam("runner_id"), IssueID: c.QueryParam("issue_id"), AttemptID: c.QueryParam("attempt_id"), Cursor: c.QueryParam("cursor")}
	if value := c.QueryParam("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			return s.nativeAPIError(c, nativeInvalid("Invalid diagnostic limit"))
		}
		args.Limit = n
	}
	for key := range c.QueryParams() {
		if !slices.Contains([]string{"runner_id", "issue_id", "attempt_id", "cursor", "limit"}, key) {
			return s.nativeAPIError(c, nativeInvalid("Invalid diagnostic selector"))
		}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, err := operatortool.DecodeLocalProjectArguments(operatortool.RunnerProjectDiagnostics, raw, true); err != nil {
		return s.nativeAPIError(c, nativeInvalid("Invalid diagnostic selector"))
	}
	page, err := s.readRunnerProjectDiagnostics(c.Request().Context(), scope, args)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}

func (s *Service) readRunnerProjectDiagnostics(ctx context.Context, scope nativeScope, args operatortool.LocalProjectArguments) (runnerauth.DiagnosticPage, error) {
	scope.project = tracker.ProjectID(args.ProjectID)
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	defer tx.Rollback()
	if err := authorizeNativeProject(ctx, tx, scope); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, json_extract(project_configuration_json, ?) FROM runner_identities WHERE organization_id = ? AND EXISTS(SELECT 1 FROM token_grants WHERE token_id = runner_identities.token_id AND project_id = ?) AND (? = '' OR id = ?) ORDER BY id LIMIT 101", "$."+jsonPathKey(args.ProjectID), scope.organization, scope.project, args.RunnerID, args.RunnerID)
	if err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	defer rows.Close()
	type observation struct {
		id  string
		raw *string
	}
	var observed []observation
	for rows.Next() {
		var r observation
		if err := rows.Scan(&r.id, &r.raw); err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		observed = append(observed, r)
	}
	if err := rows.Err(); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	if err := rows.Close(); err != nil {
		return runnerauth.DiagnosticPage{}, err
	}
	page, err := (*runnerauth.ProjectDiagnostics)(nil).Page(args.ProjectID, "", args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
	if err != nil {
		return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
	}
	found := false
	for _, item := range observed {
		r, err := readRunner(ctx, tx, scope.organization, item.id, now)
		if err != nil {
			return runnerauth.DiagnosticPage{}, err
		}
		if !slices.Contains(r.ProjectIDs, scope.project) {
			continue
		}
		if found {
			return runnerauth.DiagnosticPage{}, &operatortool.RequestError{Code: "invalid_request", Message: "Multiple runners report this project; select runner_id"}
		}
		found = true
		var view runnerauth.ProjectConfiguration
		if item.raw != nil {
			if err := json.Unmarshal([]byte(*item.raw), &view); err != nil {
				return runnerauth.DiagnosticPage{}, errProjectServiceUnavailable
			}
		}
		if view.Diagnostics != nil {
			view.Diagnostics.Redact()
		}
		page, err = view.Diagnostics.Page(args.ProjectID, r.RunnerID, args.IssueID, args.AttemptID, args.Cursor, args.Limit, now)
		if err != nil {
			return runnerauth.DiagnosticPage{}, operatortool.ErrInvalidArguments
		}
		page.ReceivedAt = view.ObservedAt
		if r.ConnectionHealth != "online" {
			page.Status = "offline"
			page.Unavailable["runner_reachability"] = "offline_or_revoked"
		}
	}
	if !found {
		page.Unavailable["runner"] = "not_authorized_or_unreported"
	}
	if len(observed) > 100 {
		page.Truncated = true
		page.Unavailable["runner_inventory"] = "truncated_select_runner_id"
	}
	return page, nil
}
