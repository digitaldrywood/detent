package orchestrator

import (
	"context"
	"time"
)

type RefreshResponse struct {
	Queued      bool      `json:"queued"`
	Coalesced   bool      `json:"coalesced"`
	RequestedAt time.Time `json:"requested_at"`
	Operations  []string  `json:"operations"`
}

func (o *Orchestrator) RequestRefresh(ctx context.Context) (RefreshResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	response := RefreshResponse{
		Queued:      true,
		RequestedAt: time.Now().UTC().Truncate(time.Second),
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
	case o.refreshes <- response.RequestedAt:
		return response, nil
	default:
		response.Coalesced = true
		return response, nil
	}
}
