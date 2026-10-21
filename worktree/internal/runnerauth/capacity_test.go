package runnerauth

import (
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

func TestRunnerCapacityEvidence(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name      string
		change    func(*Runner)
		effective int
		status    string
	}{
		{"fully applied", func(*Runner) {}, 6, "applied"},
		{"default report", func(r *Runner) { r.ProviderCapacity[0].MaxConcurrent = 0; r.ProviderCapacity[0].State = "unknown" }, 6, "applied"},
		{"external producer bound", func(r *Runner) { r.ProviderCapacity[0].MaxConcurrent = 2 }, 2, "partially_applied"},
		{"immutable local bound", func(r *Runner) { r.CapacityConfig.Manageable = false; r.CapacityConfig.LocalLimit = 2 }, 2, "partially_applied"},
		{"reload pending", func(r *Runner) { r.CapacityConfig.RuntimeLimit = 2 }, 2, "partially_applied"},
		{"host bound", func(r *Runner) { r.HostCapacity = 2 }, 2, "partially_applied"},
		{"missing configuration", func(r *Runner) { r.CapacityConfig = nil }, -1, "unknown"},
		{"stale configuration", func(r *Runner) { r.CapacityConfig.ObservedAt = now.Add(-HeartbeatTimeout) }, -1, "unknown"},
		{"stale heartbeat", func(r *Runner) { r.LastHeartbeatAt = now.Add(-HeartbeatTimeout) }, -1, "unknown"},
		{"stale provider", func(r *Runner) { r.ProviderCapacity[0].ObservedAt = now.Add(-providercapacity.MaxAge) }, -1, "unknown"},
		{"future provider", func(r *Runner) { r.ProviderCapacity[0].ObservedAt = now.Add(time.Second) }, -1, "unknown"},
		{"missing provider", func(r *Runner) { r.ProviderCapacity = nil }, -1, "unknown"},
		{"multiple provider accounts", func(r *Runner) {
			other := r.ProviderCapacity[0]
			other.AccountAlias = "other"
			r.ProviderCapacity = append(r.ProviderCapacity, other)
		}, -1, "unknown"},
		{"provider exhaustion", func(r *Runner) { r.ProviderCapacity[0].State = "exhausted" }, 0, "partially_applied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{Binding: Binding{RunnerID: "runner"}, Routing: Routing{CapacityLimit: 6}, HostCapacity: 6, ReportedCapacity: 6, LastHeartbeatAt: now, Health: "online", ConnectionHealth: "online",
				CapacityConfig:   &CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 6, ClientLimit: 6, RuntimeLimit: 6, Manageable: true, ObservedAt: now},
				ProviderCapacity: []providercapacity.View{{Report: providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", MaxConcurrent: 6, ObservedAt: now}, State: "available"}},
			}
			test.change(&r)
			view := r.CapacityView("codex", now)
			if view.Status != test.status || test.effective < 0 && view.Effective != nil || test.effective >= 0 && (view.Effective == nil || *view.Effective != test.effective) {
				t.Fatalf("view=%+v", view)
			}
		})
	}
}

func TestRunnerCapacityApprovalClassification(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name   string
		change func(*Runner)
		want   bool
	}{
		{"recorded mismatch", func(*Runner) {}, true},
		{"heartbeat expires", func(r *Runner) { r.LastHeartbeatAt = now.Add(-HeartbeatTimeout) }, true},
		{"observation expires", func(r *Runner) { r.CapacityConfig.ObservedAt = now.Add(-HeartbeatTimeout) }, true},
		{"missing configuration", func(r *Runner) { r.CapacityConfig = nil }, false},
		{"configuration applied", func(r *Runner) { r.CapacityConfig.LocalLimit, r.CapacityConfig.ClientLimit = 6, 6 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{LastHeartbeatAt: now, CapacityConfig: &CapacityConfig{LocalLimit: 2, ClientLimit: 2, ObservedAt: now}}
			test.change(&r)
			if got := r.CapacityRequiresApplication(6); got != test.want {
				t.Fatalf("material configuration change=%t, want %t", got, test.want)
			}
		})
	}
}
