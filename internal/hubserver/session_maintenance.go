package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func (s *Service) activeHostedSessions() bool {
	if s.conversations != nil {
		if coordinator, ok := s.conversations.Coordinator.(*conversationTurnCoordinator); ok {
			coordinator.mu.Lock()
			active := coordinator.workers > 0 || len(coordinator.held) > 0
			coordinator.mu.Unlock()
			if active {
				return true
			}
		}
	}
	return s.workspaces != nil && s.workspaces.relay.Active()
}

func (s *Service) sessionMaintenanceFeatures() []hubmaintenance.Feature {
	org := s.config.runtimeTenant
	c, w := s.conversations, s.workspaces
	return []hubmaintenance.Feature{
		s.cursorMaintenance("conversation-restart", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			start := s.config.runtimeHost.startedAt.UTC().Truncate(time.Microsecond)
			if !cursor.At.Equal(start) {
				*cursor = postgres.MaintenanceCursor{At: start}
			}
			if cursor.Kind == "complete" {
				return 0, nil
			}
			next, done, used, err := c.RecoverPage(ctx, org, cursor.Key, start, limit)
			if err == nil {
				cursor.Key = next
				if done {
					cursor.Kind = "complete"
				}
			}
			return used, err
		}),
		s.cursorMaintenance("conversation-execution", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			next, used, err := c.ReconcilePage(ctx, org, cursor.Key, 1, limit)
			cursor.Key = next
			return used, err
		}),
		s.cursorMaintenance("conversation-pending", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			next, used, err := c.WakePage(ctx, org, cursor.Key, limit, 1)
			cursor.Key = next
			return used, err
		}),
		s.cursorMaintenance("conversation-settle", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			next, used, _, err := c.SettlePage(ctx, org, cursor.Key, s.config.Clock(), limit)
			cursor.Key = next
			return used, err
		}),
		s.cursorMaintenance("conversation-compaction", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			next, used, _, err := c.CompactPage(ctx, org, cursor.Key, s.config.Clock(), 1, limit)
			cursor.Key = next
			return used, err
		}),
		maintenanceFeature("conversation-attachments", func(ctx context.Context, limit int) (int, error) {
			if c == nil {
				return 0, nil
			}
			used, _, err := c.SweepAttachmentPage(ctx, limit)
			return used, err
		}),
		s.cursorMaintenance("workspace-deadlines", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if w == nil {
				return 0, nil
			}
			rows, err := w.lifecycle.LimitedPage(ctx, cursor.At, cursor.Key, limit)
			if err != nil {
				return 0, err
			}
			now := w.now()
			processed := 0
			for _, row := range rows {
				occupied := w.relay.Room(row.ID) != nil
				if occupied && processed > 0 {
					break
				}
				recovery := row.UpdatedAt.Before(s.config.runtimeHost.startedAt.Truncate(time.Microsecond)) && (row.State != workspacesession.StateRequested || row.RebindDeadline != nil)
				moved, changed, err := w.lifecycle.ApplyLifecycle(ctx, row, now, recovery)
				if err != nil {
					return len(rows), err
				}
				if changed {
					w.committed(ctx, workspaceRecord(moved))
				}
				cursor.At, cursor.Key = row.ExpiresAt, row.ID
				processed++
				if occupied {
					break
				}
			}
			if processed == len(rows) && len(rows) < limit {
				*cursor = postgres.MaintenanceCursor{}
			}
			return len(rows), nil
		}),
		s.cursorMaintenance("workspace-action-runs", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if w == nil {
				return 0, nil
			}
			return w.sweepActionPage(ctx, cursor, limit)
		}),
		s.cursorMaintenance("workspace-dispatch", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if w == nil {
				return 0, nil
			}
			return w.dispatchActionPage(ctx, cursor, limit)
		}),
		s.cursorMaintenance("workspace-tickets", func(ctx context.Context, cursor *postgres.MaintenanceCursor, limit int) (int, error) {
			if w == nil {
				return 0, nil
			}
			return w.sweepTicketPage(ctx, cursor, w.now(), limit)
		}),
		maintenanceFeature("workspace-relay", func(ctx context.Context, limit int) (int, error) {
			if w == nil {
				return 0, nil
			}
			return w.relay.SweepPage(ctx, w.now(), limit), nil
		}),
	}
}
