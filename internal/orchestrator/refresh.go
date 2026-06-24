package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type RefreshResponse struct {
	RequestID   string                         `json:"request_id,omitempty"`
	Status      telemetry.RefreshAttemptStatus `json:"status,omitempty"`
	Queued      bool                           `json:"queued"`
	Coalesced   bool                           `json:"coalesced"`
	RequestedAt time.Time                      `json:"requested_at"`
	Operations  []string                       `json:"operations"`
}

type manualRefreshRequest struct {
	id          string
	requestedAt time.Time
	operations  []string
}

func (o *Orchestrator) RequestRefresh(ctx context.Context) (RefreshResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	requestedAt := time.Now().UTC()
	response := RefreshResponse{
		RequestID:   o.nextManualRefreshID(requestedAt),
		Status:      telemetry.RefreshAttemptStatusInProgress,
		Queued:      true,
		RequestedAt: requestedAt,
		Operations:  []string{"poll", "reconcile"},
	}

	select {
	case <-ctx.Done():
		return RefreshResponse{}, ctx.Err()
	case <-o.done:
		return RefreshResponse{}, ErrStopped
	default:
	}

	select {
	case <-ctx.Done():
		return RefreshResponse{}, ctx.Err()
	case <-o.done:
		return RefreshResponse{}, ErrStopped
	case o.refreshes <- manualRefreshRequest{
		id:          response.RequestID,
		requestedAt: response.RequestedAt,
		operations:  append([]string(nil), response.Operations...),
	}:
		return response, nil
	default:
		response.Coalesced = true
		response.Status = telemetry.RefreshAttemptStatusCoalesced
		return response, nil
	}
}

func (o *Orchestrator) nextManualRefreshID(requestedAt time.Time) string {
	return fmt.Sprintf("manual-%d-%d", requestedAt.UnixNano(), o.refreshSeq.Add(1))
}
