package claimapi

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrNoClaimableWork = errors.New("no compatible work item is claimable")

func (d Deps) TrackerAPIError(c echo.Context, err error) error {
	status := http.StatusInternalServerError
	code := "hub_operation_failed"
	message := "Hub operation failed"
	switch {
	case errors.Is(err, ErrNoClaimableWork):
		status, code, message = http.StatusConflict, "no_claimable_work", "No compatible work item is claimable"
	case errors.Is(err, tracker.ErrWorkItemNotFound), errors.Is(err, tracker.ErrInvalidWorkItemID):
		status, code, message = http.StatusNotFound, "work_item_not_found", "Work item was not found"
	case errors.Is(err, tracker.ErrMachineNotFound):
		status, code, message = http.StatusNotFound, "machine_not_found", "Machine was not found"
	case errors.Is(err, tracker.ErrLeaseNotFound):
		status, code, message = http.StatusNotFound, "lease_not_found", "Lease was not found"
	case errors.Is(err, tracker.ErrLeaseConflict):
		status, code, message = http.StatusConflict, "lease_conflict", "Work item is already leased"
	case errors.Is(err, tracker.ErrStaleFencingToken):
		status, code, message = http.StatusConflict, "stale_fencing_token", "Lease fencing token is stale"
	case errors.Is(err, tracker.ErrInvalidClaimRequest), errors.Is(err, tracker.ErrInvalidLeaseRequest), errors.Is(err, tracker.ErrInvalidWorkEvent), errors.Is(err, tracker.ErrInvalidCandidateQuery):
		status, code, message = http.StatusUnprocessableEntity, "invalid_request", "Request is invalid"
	}
	return d.RefusedAPIError(c, status, code, message, hubapi.APIErrorResponse{Code: code, Message: message})
}

// claimPreflightAcquire bounds how long a preflight waits for a claim reader;
// a busy pool sends the claim down the full write pass instead of queueing.

// claimMode selects how one claim pass runs: a preflight on the reader that
// stops at the first claimable candidate, a write pass restricted to that
// candidate, or the full write pass.
