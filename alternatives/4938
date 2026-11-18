package runnerauth

import (
	"testing"
	"time"
)

func TestLifecycleHealth(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name               string
		state              LifecycleState
		age                time.Duration
		health, connection string
	}{
		{"starting", LifecycleStarting, 0, "offline", "offline"},
		{"ready deadline boundary", LifecycleStarting, 2 * time.Minute, "needs_attention", "offline"},
		{"ready before deadline", LifecycleStarting, 2*time.Minute - time.Nanosecond, "offline", "offline"},
		{"ready", LifecycleReady, 0, "online", "online"},
		{"failed", LifecycleStartFailed, 0, "needs_attention", "offline"},
		{"shutting down", LifecycleShuttingDown, 0, "offline", "offline"},
		{"stopped", LifecycleStopped, 0, "offline", "offline"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{Health: "online", ConnectionHealth: "online", Lifecycle: &Lifecycle{LifecycleReport: LifecycleReport{State: test.state, Version: "1.2.3"}, LastTransitionAt: now.Add(-test.age)}}
			r.ApplyLifecycle(now)
			if test.state == LifecycleStarting && test.age == 2*time.Minute && !r.Lifecycle.LastTransitionAt.Equal(now) {
				t.Fatalf("derived transition time=%s want %s", r.Lifecycle.LastTransitionAt, now)
			}
			if r.Health != test.health || r.ConnectionHealth != test.connection || r.Lifecycle.Version != "1.2.3" {
				t.Fatalf("health=%s connection=%s lifecycle=%+v", r.Health, r.ConnectionHealth, r.Lifecycle)
			}
		})
	}
	t.Run("legacy heartbeat", func(t *testing.T) {
		r := Runner{Health: "online", ConnectionHealth: "online"}
		r.ApplyLifecycle(now)
		if r.Health != "online" || r.Lifecycle != nil {
			t.Fatalf("legacy runner=%+v", r)
		}
	})
}
