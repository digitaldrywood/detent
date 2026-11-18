package dispatch

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
)

func (s *Scheduler) RestoreCompletion(ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, saved json.RawMessage) (orchestrator.Claimed, error) {
	if s.completionRestorer == nil {
		return orchestrator.Claimed{}, runner.ErrExecutionAuthorityUnavailable
	}
	return s.completionRestorer(s, ctx, request, issue, saved)
}
