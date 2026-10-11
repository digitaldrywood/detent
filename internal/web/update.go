package web

import (
	"context"
	"errors"
	"html"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/mutation"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type UpdateApplier interface {
	ApplyPending(context.Context) (detentupdate.Status, error)
}

type updateApplyRequest struct {
	Confirm     bool `json:"confirm" form:"confirm"`
	Release     bool `json:"release" form:"release"`
	FromRelease bool `json:"from_release" form:"from_release"`
}

type updateApplyResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

func (s *Server) apiUpdateApply(c echo.Context) error {
	if s.updateApplier == nil {
		return updateApplyError(c, http.StatusServiceUnavailable, "update_unavailable", "Update apply is unavailable")
	}
	var request updateApplyRequest
	if err := c.Bind(&request); err != nil {
		return updateApplyError(c, http.StatusUnprocessableEntity, "invalid_request", "Request body must be valid JSON or form data")
	}
	if !request.Confirm {
		return updateApplyError(c, http.StatusPreconditionRequired, "confirmation_required", "Confirm the update restart with confirm=true")
	}

	status, err := s.applyOperatorUpdate(c.Request().Context(), request.Release, request.FromRelease)
	if err != nil {
		var problem *controlProblem
		if errors.As(err, &problem) {
			return updateApplyError(c, problem.status, problem.code, problem.message)
		}
		return updateApplyError(c, http.StatusServiceUnavailable, "update_unavailable", "Update apply is unavailable")
	}

	response := updateApplyResponse{Status: "applying", Version: status.LatestVersion}
	if htmxRequest(c) {
		return c.HTML(http.StatusAccepted, `<span class="font-medium text-ok">Update applied; Detent is restarting.</span>`)
	}
	if request.Release {
		return c.JSON(http.StatusAccepted, status)
	}
	return c.JSON(http.StatusAccepted, response)
}

func (s *Server) applyOperatorUpdate(ctx context.Context, release, fromRelease bool) (detentupdate.Status, error) {
	if s.updateApplier == nil {
		return detentupdate.Status{}, errOperatorCommandUnavailable
	}
	var status detentupdate.Status
	var err error
	if release {
		applier, ok := s.updateApplier.(interface {
			ApplyRelease(context.Context, bool) (detentupdate.Status, error)
		})
		if !ok {
			return detentupdate.Status{}, &controlProblem{http.StatusServiceUnavailable, "update_unavailable", "Runtime release apply is unavailable"}
		}
		status, err = applier.ApplyRelease(ctx, fromRelease)
	} else {
		status, err = s.updateApplier.ApplyPending(ctx)
	}
	if err != nil {
		if errors.Is(err, detentupdate.ErrNoPendingUpdate) {
			return detentupdate.Status{}, &controlProblem{http.StatusConflict, "update_not_pending", "No Detent update is pending"}
		}
		s.logger.Error("apply pending Detent update failed", "error", mutation.ErrorText(ctx, err))
		if errors.Is(err, detentupdate.ErrRefused) && status.InstallSource == detentupdate.InstallSourceUnknown {
			return detentupdate.Status{}, &controlProblem{http.StatusConflict, "update_apply_failed", "Detent cannot verify this installation's owner. Set DETENT_INSTALL_LOCK or DETENT_STATE_DIR in the running service to its existing installer receipt and retry"}
		}
		return detentupdate.Status{}, &controlProblem{http.StatusInternalServerError, "update_apply_failed", "Detent update apply failed"}
	}
	return status, nil
}

func updateApplyError(c echo.Context, status int, code string, message string) error {
	if htmxRequest(c) {
		return c.HTML(status, `<span class="font-medium text-err">`+html.EscapeString(message)+`.</span>`)
	}
	return c.JSON(status, errorResponse(code, message))
}
