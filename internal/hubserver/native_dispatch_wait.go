package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func dispatchNotificationKey(organization tracker.OrganizationID) string {
	return "dispatch:" + string(organization)
}

func (b *notificationBroker) dispatchCursor(key string) string {
	return b.Cursor(key)
}

func (s *Service) notifyNativeDispatch(ctx context.Context, scope hubscope.NativeScope, input any) {
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type != "run.finished" {
		return
	}
	s.notifications.notify(dispatchNotificationKey(scope.Organization))
	s.stopMonthlyBudgetTurns(ctx, scope.Organization)
	s.stopMonthlyBudgetSpritePasses(ctx, scope.Organization)
	switch input.(type) {
	case monthlyBudgetRequest, spriteUsageRequest:
		s.resumeMonthlyBudgetWork(ctx, scope)
	case tracker.NativeRunEvent:
		if s.conversations != nil {
			if err := s.conversations.WakePending(ctx); err != nil {
				s.config.Logger.Warn("Resume cost-corrected conversations", "error", err)
			}
		}
	}
	if event, ok := input.(tracker.NativeRunEvent); ok && event.Type == "run.finished" && scope.Project != "" {
		s.startSpritePoolForQueue(ctx, scope)
	}
}

func (s *Service) signalNativeDispatch(scope hubscope.NativeScope) {
	s.notifications.notify(dispatchNotificationKey(scope.Organization))
	select {
	case s.boardRefresh <- struct{}{}:
	default:
	}
}

func newDispatchWaitTimer(wait time.Duration) *time.Timer {
	return time.NewTimer(wait)
}
