package hubserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type mutationResponse struct {
	WorkItemID tracker.WorkItemID `json:"work_item_id"`
	Kind       string             `json:"kind"`
	Status     string             `json:"status"`
	OutboxID   int64              `json:"outbox_id,omitempty"`
}

func (s *Service) operatorAPIError(c echo.Context, err error) error {
	status := http.StatusUnprocessableEntity
	code := "invalid_mutation"
	message := "Mutation is invalid"
	switch {
	case errors.Is(err, tracker.ErrWorkItemNotFound):
		status, code, message = http.StatusNotFound, "work_item_not_found", "Work item was not found"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "idempotency_conflict", "Idempotency key conflicts with an earlier mutation"
	case strings.Contains(err.Error(), "not found"):
		status, code, message = http.StatusNotFound, "mutation_target_not_found", "Mutation target was not found"
	}
	return s.refusedAPIError(c, status, code, message, apiErrorResponse{Code: code, Message: message})
}

func queuePriority(value string) (int, bool) {
	switch value {
	case "urgent":
		return tracker.QueuePriorityUrgent, true
	case "high":
		return tracker.QueuePriorityHigh, true
	case "normal":
		return tracker.QueuePriorityNormal, true
	case "low":
		return tracker.QueuePriorityLow, true
	case "unset":
		return 4, true
	default:
		return 0, false
	}
}
