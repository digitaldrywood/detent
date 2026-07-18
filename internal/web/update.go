package web

import (
	"context"
	"errors"
	"html"
	"net/http"

	"github.com/labstack/echo/v4"

	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type UpdateApplier interface {
	ApplyPending(context.Context) (detentupdate.Status, error)
}

type updateApplyRequest struct {
	Confirm bool `json:"confirm" form:"confirm"`
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

	status, err := s.updateApplier.ApplyPending(c.Request().Context())
	if err != nil {
		if errors.Is(err, detentupdate.ErrNoPendingUpdate) {
			return updateApplyError(c, http.StatusConflict, "update_not_pending", "No Detent update is pending")
		}
		s.logger.Error("apply pending Detent update failed", "error", err)
		return updateApplyError(c, http.StatusInternalServerError, "update_apply_failed", "Detent update apply failed")
	}
	response := updateApplyResponse{Status: "applying", Version: status.LatestVersion}
	if htmxRequest(c) {
		return c.HTML(http.StatusAccepted, `<span class="font-medium text-ok">Update applied; Detent is restarting.</span>`)
	}
	return c.JSON(http.StatusAccepted, response)
}

func updateApplyError(c echo.Context, status int, code string, message string) error {
	if htmxRequest(c) {
		return c.HTML(status, `<span class="font-medium text-err">`+html.EscapeString(message)+`.</span>`)
	}
	return c.JSON(status, errorResponse(code, message))
}
