package scheduler

import (
	"slices"
	"strings"
)

const (
	DecisionReasonAlreadyClaimed                   = "already_claimed"
	DecisionReasonAlreadyRunning                   = "already_running"
	DecisionReasonArtifactGateWaitStatus           = "artifact_gate_wait_status"
	DecisionReasonAuthorizationSelectorDeclined    = "authorization_selector_declined"
	DecisionReasonAwaitingGate                     = "awaiting_gate"
	DecisionReasonBackendCapacityRecovery          = "backend_capacity_recovery"
	DecisionReasonBlocked                          = "blocked"
	DecisionReasonBlockedByDependency              = "blocked_by_dependency"
	DecisionReasonBudgetCooldown                   = "budget_cooldown"
	DecisionReasonBudgetHardHold                   = "budget_hard_hold"
	DecisionReasonCIUnavailable                    = "ci_unavailable"
	DecisionReasonCompletionDeferred               = "completion_deferred"
	DecisionReasonCurrentHeadCIWait                = "current_head_ci_wait"
	DecisionReasonDispatchBackoffCancelled         = "dispatch_backoff_cancelled"
	DecisionReasonDuplicatePullRequestWork         = "duplicate_pull_request_work"
	DecisionReasonForgeUnavailable                 = "forge_unavailable"
	DecisionReasonForgeUnavailableRecovery         = "forge_unavailable_recovery"
	DecisionReasonGitHubRESTCapacityPaused         = "github_rest_capacity_paused"
	DecisionReasonGitHubRESTRecovery               = "github_rest_recovery"
	DecisionReasonGitHubMonitor                    = "worker_github_budget_monitor_unavailable"
	DecisionReasonGlobalCapacityFull               = DispatchGateReasonGlobalCapacityFull
	DecisionReasonHydrateFailed                    = "hydrate_failed"
	DecisionReasonInactiveState                    = "inactive_state"
	DecisionReasonInvalidCandidate                 = "invalid_candidate"
	DecisionReasonLifetimeLimit                    = "lifetime_limit"
	DecisionReasonLocalSlotUnavailable             = "local_slot_unavailable"
	DecisionReasonMergeFairnessHeadReserved        = "merge_fairness_head_reserved"
	DecisionReasonMergeWorkerCurrentHeadCIExceeded = "merge_worker_current_head_ci_wait_exceeded"
	DecisionReasonMergedPullRequestPending         = "merged_pull_request_reconciliation_pending"
	DecisionReasonOwnershipAssigneeRequired        = "ownership_assignee_required"
	DecisionReasonProjectFailureBreakerPaused      = "project_failure_breaker_paused"
	DecisionReasonProjectFailureBreakerRecovery    = "project_failure_breaker_recovery"
	DecisionReasonProviderRateWindowBackpressure   = "provider_rate_window_backpressure"
	DecisionReasonPullRequestHydrationRecovery     = "pull_request_hydration_recovery"
	DecisionReasonPullRequestHydrationUnavailable  = "pull_request_hydration_unavailable"
	DecisionReasonRetryPending                     = "retry_pending"
	DecisionReasonTerminalState                    = "terminal_state"
	DecisionReasonTrackerUnavailable               = "tracker_unavailable"
	DecisionReasonTrackerUnavailableRecovery       = "tracker_unavailable_recovery"
	DecisionReasonWorkerHostUnavailable            = "worker_host_unavailable"
	DecisionReasonWorkspaceBranchHeld              = "workspace_branch_held"
)

var emittedDecisionReasons = []string{
	DecisionReasonAlreadyClaimed,
	DecisionReasonAlreadyRunning,
	DecisionReasonArtifactGateWaitStatus,
	DecisionReasonAuthorizationSelectorDeclined,
	DecisionReasonAwaitingGate,
	DecisionReasonBackendCapacityRecovery,
	DecisionReasonBlocked,
	DecisionReasonBlockedByDependency,
	DecisionReasonBudgetCooldown,
	DecisionReasonBudgetHardHold,
	DecisionReasonCIUnavailable,
	DecisionReasonCompletionDeferred,
	DecisionReasonCurrentHeadCIWait,
	DecisionReasonDispatchBackoffCancelled,
	DecisionReasonDuplicatePullRequestWork,
	DecisionReasonForgeUnavailable,
	DecisionReasonForgeUnavailableRecovery,
	DecisionReasonGitHubRESTCapacityPaused,
	DecisionReasonGitHubRESTRecovery,
	DecisionReasonGitHubMonitor,
	DecisionReasonHydrateFailed,
	DecisionReasonInactiveState,
	DecisionReasonInvalidCandidate,
	DecisionReasonLifetimeLimit,
	DecisionReasonLocalSlotUnavailable,
	DecisionReasonMergeFairnessHeadReserved,
	DecisionReasonMergeWorkerCurrentHeadCIExceeded,
	DecisionReasonMergedPullRequestPending,
	DecisionReasonOwnershipAssigneeRequired,
	DecisionReasonProjectFailureBreakerPaused,
	DecisionReasonProjectFailureBreakerRecovery,
	DecisionReasonProviderRateWindowBackpressure,
	DecisionReasonPullRequestHydrationRecovery,
	DecisionReasonPullRequestHydrationUnavailable,
	DecisionReasonRetryPending,
	DecisionReasonTerminalState,
	DecisionReasonTrackerUnavailable,
	DecisionReasonTrackerUnavailableRecovery,
	DecisionReasonWorkerHostUnavailable,
	DecisionReasonWorkspaceBranchHeld,
	DispatchGateReasonPaused,
	DispatchGateReasonGlobalCapacityFull,
	DispatchGateReasonOutsideActiveWindow,
	DispatchGateReasonReservedForHigherPriority,
	DispatchGateReasonReservedForHigherPriorityProject,
	DispatchGateReasonSelectedProjectWaiting,
}

var emittedDecisionReasonExamples = []string{
	DecisionReasonAlreadyRunning,
	DecisionReasonAuthorizationSelectorDeclined,
	DecisionReasonBlockedByDependency,
	DecisionReasonGitHubRESTCapacityPaused,
	DecisionReasonGitHubRESTRecovery,
	DecisionReasonGlobalCapacityFull,
	DispatchGateReasonOutsideActiveWindow,
	DecisionReasonProviderRateWindowBackpressure,
	DispatchGateReasonReservedForHigherPriorityProject,
}

func EmittedDecisionReasons() []string {
	return slices.Clone(emittedDecisionReasons)
}

func EmittedDecisionReasonExamples() []string {
	return slices.Clone(emittedDecisionReasonExamples)
}

func IsEmittedDecisionReason(reason string) bool {
	reason = strings.ToLower(strings.TrimSpace(reason))
	return slices.Contains(emittedDecisionReasons, reason)
}

func IsAuthorizationBoundaryDecisionReason(reason string) bool {
	return strings.EqualFold(strings.TrimSpace(reason), DecisionReasonAuthorizationSelectorDeclined)
}
