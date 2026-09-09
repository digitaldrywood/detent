package orchestrator

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestSchedulerDecisionWaitReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "planner project capacity wait",
			reason: dispatchSkipGlobalCapacityFull,
			want:   "project_capacity_full",
		},
		{
			name:   "slot acquisition pool capacity wait",
			reason: dispatchIssueFailureGlobalSlotUnavailable,
			want:   scheduler.DispatchGateReasonGlobalCapacityFull,
		},
		{
			name:   "lane capacity wait",
			reason: dispatchSkipLocalSlotUnavailable,
			want:   "lane_capacity_full",
		},
		{
			name:   "worker host capacity wait",
			reason: dispatchSkipWorkerHostUnavailable,
			want:   "worker_host_capacity_full",
		},
		{
			name:   "memory pressure wait",
			reason: dispatchIssueFailureMemoryPressure,
			want:   "memory pressure is above the admission threshold",
		},
		{
			name:   "IO pressure wait",
			reason: dispatchIssueFailureIOPressure,
			want:   "IO pressure is above the admission threshold",
		},
		{
			name:   "CPU pressure wait",
			reason: dispatchIssueFailureCPUPressure,
			want:   "CPU pressure is above the admission threshold",
		},
		{
			name:   "other reason",
			reason: "provider_backoff",
			want:   "provider_backoff",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := schedulerDecisionWaitReason(tt.reason); got != tt.want {
				t.Fatalf("schedulerDecisionWaitReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

func TestRecordSchedulerDependencyReasonNamesHydratedBlockers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		refs []connector.BlockedRef
		want string
	}{
		{name: "unresolved", refs: []connector.BlockedRef{{Identifier: "digitaldrywood/pyroapex#2064", State: "Todo"}}, want: "Waiting on digitaldrywood/pyroapex#2064"},
		{name: "ignore resolved", refs: []connector.BlockedRef{{Identifier: "done", State: "Done"}, {Identifier: "waiting", State: "In Progress"}}, want: "Waiting on waiting"},
		{name: "missing refs", want: "blocked_by_dependency"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
			orch := Orchestrator{cfg: cfg}
			state := newState(cfg)
			orch.recordSchedulerDecision(t.Context(), &state, time.Now(), dispatchPlanDecision{Issue: connector.Issue{ID: "todo", State: "Todo", BlockedBy: tt.refs}}, "skipped", dispatchSkipBlockedByDependency)
			if got := state.SchedulerDecisions[0].WaitReason; got != tt.want {
				t.Fatalf("wait reason = %q, want %q", got, tt.want)
			}
		})
	}
}
