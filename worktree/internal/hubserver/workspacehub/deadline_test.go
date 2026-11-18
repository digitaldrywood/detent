package workspacehub

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestWorkspaceDecideDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *time.Time { moment := now.Add(offset); return &moment }
	base := func(state string) Record {
		return Record{
			State: state, IdleTimeoutSeconds: 1800, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
			ExpiresAt: now.Add(time.Hour), RequestedExpiresAt: now.Add(time.Hour),
		}
	}
	for _, test := range []struct {
		name   string
		record func() Record
		want   Deadline
	}{
		{
			name: "a request no runner claimed in time fails with no_runner",
			record: func() Record {
				record := base(workspacesession.StateRequested)
				record.RequestedExpiresAt = now.Add(-time.Second)
				return record
			},
			want: Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonNoRunner, EndedAt: now, Apply: true},
		},
		{
			name: "a request inside the re-bind grace waits for the runner that held it",
			record: func() Record {
				record := base(workspacesession.StateRequested)
				record.RebindDeadline = at(10 * time.Second)
				return record
			},
			want: Deadline{},
		},
		{
			name: "a request past the re-bind grace fails with hub_restarted",
			record: func() Record {
				record := base(workspacesession.StateRequested)
				record.RebindDeadline = at(-time.Second)
				return record
			},
			want: Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonHubRestarted, EndedAt: now, Apply: true},
		},
		{
			name: "two missed heartbeats mark a ready workspace unreachable",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.MissedHeartbeats*workspacesession.HeartbeatInterval + time.Second))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: Deadline{State: workspacesession.StateUnreachable, Reason: workspacesession.ReasonRunnerRestarted, EndedAt: now, Apply: true},
		},
		{
			name: "a workspace one second inside the two-heartbeat window is left alone",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.MissedHeartbeats*workspacesession.HeartbeatInterval - time.Second))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: Deadline{},
		},
		{
			name: "the lease TTL itself is the lease_lost boundary",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-workspacesession.LeaseTTL)
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonLeaseLost, EndedAt: now, Apply: true},
		},
		{
			name: "an expired lease fails the workspace and ends occupancy at expiry, not at discovery",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(-(workspacesession.LeaseTTL + time.Minute))
				record.OpenedAt = at(-time.Minute)
				return record
			},
			want: Deadline{State: workspacesession.StateFailed, Reason: workspacesession.ReasonLeaseLost,
				EndedAt: now.Add(-(workspacesession.LeaseTTL + time.Minute)).Add(workspacesession.LeaseTTL), Apply: true},
		},
		{
			name: "the hard lifetime cap closes a ready workspace as expired",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Minute)
				record.ExpiresAt = now.Add(-time.Second)
				return record
			},
			want: Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true},
		},
		{
			name: "a ready workspace past its idle timeout closes as expired",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.IdleTimeoutSeconds = 60
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Hour)
				record.LastActivityAt = at(-2 * time.Minute)
				return record
			},
			want: Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true},
		},
		{
			name: "a ready workspace past the idle mark only reports idle",
			record: func() Record {
				record := base(workspacesession.StateReady)
				record.LastHeartbeatAt = at(0)
				record.OpenedAt = at(-time.Hour)
				record.LastActivityAt = at(-(IdleMark + time.Second))
				return record
			},
			want: Deadline{State: workspacesession.StateIdle, EndedAt: now, Apply: true},
		},
		{
			name: "an unreachable workspace closes after its idle timeout",
			record: func() Record {
				record := base(workspacesession.StateUnreachable)
				record.IdleTimeoutSeconds = 60
				record.UpdatedAt = now.Add(-2 * time.Minute)
				return record
			},
			want: Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonExpired, EndedAt: now, Apply: true},
		},
		{
			name: "a runner that never answers a close stops holding the slot at the lease TTL",
			record: func() Record {
				record := base(workspacesession.StateClosing)
				record.Reason = workspacesession.ReasonClosedByActor
				record.UpdatedAt = now.Add(-(workspacesession.LeaseTTL + time.Second))
				return record
			},
			want: Deadline{State: workspacesession.StateClosed, Reason: workspacesession.ReasonClosedByActor, EndedAt: now, Apply: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := DecideDeadline(test.record(), now)
			if got.State != test.want.State || got.Reason != test.want.Reason || got.Apply != test.want.Apply || !got.EndedAt.Equal(test.want.EndedAt) {
				t.Fatalf("DecideDeadline() = %+v, want %+v", got, test.want)
			}
		})
	}
}
