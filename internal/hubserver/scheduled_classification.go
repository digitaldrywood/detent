package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/operatorcommands"
)

func (s *Service) scheduledRepository(ctx context.Context, scope hubscope.NativeScope) (string, error) {
	return operatorcommands.ScheduledRepository(ctx, s.config.TenantRepository, scope)
}

func (s *Service) classifyScheduledFailure(c echo.Context) error {
	var text operatorcommands.CITextBackend
	if cfg := s.config.Conversation; cfg != nil {
		text = operatorcommands.CITextBackend{
			Backend: cfg.Backend, Workspace: cfg.Workspace, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		}
	}
	deps := operatorcommands.ScheduledCIDeps{
		Repository: s.config.TenantRepository,
		Now:        s.config.Clock,
		Text:       text,
		Error:      func(c echo.Context, err error) error { return s.nativeAPIError(c, err) },
		Invalid:    s.invalidAPIRequest,
	}
	return deps.Classify(c)
}
