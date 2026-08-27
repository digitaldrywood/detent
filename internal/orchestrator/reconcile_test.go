package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/provenance"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/staleness"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestTickReconcilesRunningIssueTrackerState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	prNumber := 226
	prior := connector.Issue{
		ID:         "issue-running",
		Identifier: "digitaldrywood/detent#225",
		Title:      "Dispatch title",
		State:      "Todo",
		URL:        "https://github.com/digitaldrywood/detent/issues/225",
		Labels:     []string{"bug"},
		PRNumber:   &prNumber,
		PullRequest: &connector.PullRequest{
			Number:           prNumber,
			URL:              "https://github.com/digitaldrywood/detent/pull/226",
			BranchName:       "detent/digitaldrywood_detent_225",
			State:            "OPEN",
			CIStatus:         "success",
			CodexReviewState: "COMMENTED",
		},
	}

	tests := []struct {
		name       string
		tracker    []connector.Issue
		err        error
		wantState  string
		wantTitle  string
		wantURL    string
		wantLabels []string
	}{
		{
			name: "updates running issue from tracker",
			tracker: []connector.Issue{
				{
					ID:         prior.ID,
					Identifier: prior.Identifier,
					Title:      "Live title",
					State:      "In Progress",
					URL:        "https://github.com/digitaldrywood/detent/issues/225#live",
					Labels:     []string{"bug", "live"},
				},
			},
			wantState:  "In Progress",
			wantTitle:  "Live title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225#live",
			wantLabels: []string{"bug", "live"},
		},
		{
			name:       "retains previous running issue on fetch error",
			err:        errors.New("tracker unavailable"),
			wantState:  "Todo",
			wantTitle:  "Dispatch title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225",
			wantLabels: []string{"bug"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			state.Running[prior.ID] = Running{Issue: cloneIssue(prior)}
			state.Claimed[prior.ID] = Claimed{Issue: cloneIssue(prior)}

			tracker := &runningStateConnector{
				issues: tt.tracker,
				err:    tt.err,
			}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			snapshot := state.Snapshot(now)
			if len(snapshot.Running) != 1 {
				t.Fatalf("Running len = %d, want 1", len(snapshot.Running))
			}
			got := snapshot.Running[0].Issue
			if got.State != tt.wantState {
				t.Fatalf("running snapshot state = %q, want %q", got.State, tt.wantState)
			}
			if got.Title != tt.wantTitle {
				t.Fatalf("running snapshot title = %q, want %q", got.Title, tt.wantTitle)
			}
			if got.URL != tt.wantURL {
				t.Fatalf("running snapshot URL = %q, want %q", got.URL, tt.wantURL)
			}
			if !slices.Equal(got.Labels, tt.wantLabels) {
				t.Fatalf("running snapshot labels = %#v, want %#v", got.Labels, tt.wantLabels)
			}
			if got.PullRequest == nil {
				t.Fatal("running snapshot pull request = nil, want preserved metadata")
			}
			if got.PullRequest.URL != "https://github.com/digitaldrywood/detent/pull/226" {
				t.Fatalf("running snapshot pull request URL = %q, want preserved metadata", got.PullRequest.URL)
			}
			if got.PullRequest.CIStatus != "success" || got.PullRequest.CodexReviewState != "COMMENTED" {
				t.Fatalf("running snapshot pull request status = %#v, want preserved metadata", got.PullRequest)
			}
			if !slices.Equal(tracker.requestedIDs, []string{prior.ID}) {
				t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want [%s]", tracker.requestedIDs, prior.ID)
			}
		})
	}
}

func TestReconcileRunningIssuesStopsWorkerOutsideActiveLane(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 16, 18, 35, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:         "wi-0c7d736611a111641bd57b97",
		Identifier: "digitaldrywood/video-studio#22",
		State:      "Production",
	}
	parked := cloneIssue(issue)
	parked.State = "Blocked"
	runCtx, stop := context.WithCancelCause(context.Background())
	state := newState(normalizeConfig(Config{
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Blocked", "Review"},
		TerminalStates: []string{"Done", "Cancelled"},
	}))
	state.Running[issue.ID] = Running{Issue: issue, stop: stop}
	tracker := &runningStateConnector{issues: []connector.Issue{parked}}
	orch := &Orchestrator{
		cfg:       normalizeConfig(Config{ActiveStates: []string{"Todo", "Production", "Rework"}}),
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.reconcileRunningIssues(t.Context(), &state, now)

	select {
	case <-runCtx.Done():
	default:
		t.Fatal("worker context remains active after the item moved to Blocked")
	}
}

func TestTrackBlockedStatusIssuesResolvesCauseByPrecedence(t *testing.T) {
	t.Parallel()

	parkedAt := time.Date(2026, 8, 27, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name             string
		issue            connector.Issue
		runtimeCause     string
		runtimeRecovery  string
		recoveryAction   string
		recoveryReason   string
		detentCause      string
		revocationCause  string
		revocationOrigin provenance.Origin
		wantReason       string
		wantOldAbsent    bool
	}{
		{
			name: "live Detent park record",
			issue: connector.Issue{
				ID:            "issue-live-detent-park",
				Identifier:    "digitaldrywood/detent#1994",
				State:         blockedStatusState,
				BlockerReason: "tracker fallback",
			},
			runtimeCause:    "session token ceiling exceeded",
			runtimeRecovery: "token_ceiling_circuit_breaker",
			wantReason:      "session token ceiling exceeded",
			wantOldAbsent:   true,
		},
		{
			name: "persisted Detent park after restart",
			issue: connector.Issue{
				ID:            "issue-detent-park",
				Identifier:    "digitaldrywood/detent#1995",
				State:         blockedStatusState,
				BlockerReason: "tracker fallback",
				BlockedBy: []connector.BlockedRef{{
					Identifier: "digitaldrywood/detent#1800",
					State:      "In Progress",
				}},
			},
			detentCause:   "rework_limit",
			wantReason:    "rework_limit",
			wantOldAbsent: true,
		},
		{
			name: "recovery policy is not a blocked cause",
			issue: connector.Issue{
				ID:         "issue-recovery-policy",
				Identifier: "digitaldrywood/detent#1996",
				State:      blockedStatusState,
			},
			runtimeCause:   staleness.ReasonBlockedOutsideDetent,
			recoveryAction: "hold",
			recoveryReason: "no_recovery_predicate",
			wantReason:     staleness.ReasonBlockedOutsideDetent,
			wantOldAbsent:  true,
		},
		{
			name: "tracker authored block",
			issue: connector.Issue{
				ID:            "issue-tracker-block",
				Identifier:    "digitaldrywood/detent#1996",
				State:         blockedStatusState,
				BlockerReason: "waiting for operator approval",
				BlockedBy: []connector.BlockedRef{{
					Identifier: "digitaldrywood/detent#1800",
					State:      "In Progress",
				}},
			},
			wantReason: "waiting for operator approval",
		},
		{
			name: "dependency block",
			issue: connector.Issue{
				ID:         "issue-dependency-block",
				Identifier: "digitaldrywood/detent#1997",
				State:      blockedStatusState,
				BlockedBy: []connector.BlockedRef{{
					Identifier: "digitaldrywood/detent#1800",
					State:      "In Progress",
				}},
			},
			wantReason: blockedReasonDependency,
		},
		{
			name: "external block without tracker cause",
			issue: connector.Issue{
				ID:         "issue-external-block",
				Identifier: "digitaldrywood/detent#1998",
				State:      blockedStatusState,
			},
			wantReason:    "blocked outside Detent; no cause recorded by the tracker",
			wantOldAbsent: true,
		},
		{
			name: "lane revocation event",
			issue: connector.Issue{
				ID:         "issue-lane-revocation",
				Identifier: "digitaldrywood/detent#1999",
				State:      blockedStatusState,
			},
			revocationCause:  "current tracker lane is not worker-owned",
			revocationOrigin: provenance.OriginDetent,
			wantReason:       "current tracker lane is not worker-owned",
			wantOldAbsent:    true,
		},
		{
			name: "external tracker cause survives stale completion",
			issue: connector.Issue{
				ID:            "issue-external-stale-completion",
				Identifier:    "digitaldrywood/detent#2000",
				State:         blockedStatusState,
				BlockerReason: "waiting for operator approval",
			},
			revocationCause:  "worker lease is no longer active",
			revocationOrigin: provenance.OriginHuman,
			wantReason:       "waiting for operator approval",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			metrics := &autoPromoteWorkflowMetricsRecorder{}
			if tt.detentCause != "" {
				metadata := workflowLaneMetadata{
					BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{Cause: tt.detentCause},
				}
				if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), store.WorkflowPhaseEvent{
					ProjectID:    defaultWorkflowMetricsProjectID,
					IssueID:      tt.issue.ID,
					Identifier:   tt.issue.Identifier,
					PhaseType:    store.WorkflowPhaseTypeLane,
					PhaseName:    blockedStatusState,
					Reason:       tt.detentCause,
					Status:       "entered",
					StartedAt:    parkedAt,
					MetadataJSON: workflowLaneMetadataJSON(tt.issue, metadata),
				}); err != nil {
					t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
				}
			} else if tt.revocationCause != "" {
				source := provenance.SourceDetentInstance
				if tt.revocationOrigin == provenance.OriginHuman {
					source = provenance.SourceHumanSession
				}
				metadata := workflowLaneMetadata{
					Provenance: provenance.AttributionFromSource(source, provenance.Actor{}),
				}
				for _, event := range []store.WorkflowPhaseEvent{
					{
						ProjectID:    defaultWorkflowMetricsProjectID,
						IssueID:      tt.issue.ID,
						Identifier:   tt.issue.Identifier,
						PhaseType:    store.WorkflowPhaseTypeLane,
						PhaseName:    blockedStatusState,
						Reason:       "tracker_state_observed",
						Status:       "entered",
						StartedAt:    parkedAt,
						MetadataJSON: workflowLaneMetadataJSON(tt.issue, metadata),
					},
					{
						ProjectID:  defaultWorkflowMetricsProjectID,
						IssueID:    tt.issue.ID,
						Identifier: tt.issue.Identifier,
						PhaseType:  store.WorkflowPhaseTypeRecovery,
						PhaseName:  "stale_completion_rejected",
						Reason:     tt.revocationCause,
						Status:     "rejected",
						StartedAt:  parkedAt.Add(time.Second),
						FinishedAt: parkedAt.Add(time.Second),
					},
				} {
					if _, err := metrics.RecordWorkflowPhaseEvent(t.Context(), event); err != nil {
						t.Fatalf("RecordWorkflowPhaseEvent() error = %v", err)
					}
				}
			}

			cfg := normalizeConfig(Config{TerminalStates: []string{"Done", "Cancelled"}})
			orch := &Orchestrator{cfg: cfg, workflowMetrics: metrics}
			state := newState(cfg)
			if tt.runtimeCause != "" {
				var recovery *workflowLaneBlockedRecoveryMetadata
				if tt.runtimeRecovery != "" {
					recovery = &workflowLaneBlockedRecoveryMetadata{Cause: tt.runtimeRecovery}
				}
				state.Blocked[tt.issue.ID] = Blocked{
					Issue:          tt.issue,
					Reason:         tt.runtimeCause,
					RecoveryAction: tt.recoveryAction,
					RecoveryReason: tt.recoveryReason,
					Source:         BlockedSourceProjectStatus,
					BlockedAt:      parkedAt,
					Recovery:       recovery,
				}
			}
			orch.trackBlockedStatusIssues(t.Context(), &state, []connector.Issue{tt.issue}, parkedAt.Add(time.Minute))

			got := state.Snapshot(parkedAt.Add(2 * time.Minute)).Blocked
			if len(got) != 1 || got[0].Error != tt.wantReason {
				t.Fatalf("rendered blocked card = %#v, want reason %q", got, tt.wantReason)
			}
			if tt.wantOldAbsent && strings.Contains(got[0].Error, "cause unrecorded") {
				t.Fatalf("rendered blocked card reason = %q, want recorded cause", got[0].Error)
			}
		})
	}
}

func TestReconcileRunningIssuesRevokesIneligibleMerge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	base := connector.Issue{
		ID:         "issue-merge-revoked",
		Identifier: "digitaldrywood/detent#1434",
		State:      "Merging",
		Labels:     []string{"Ready to Merge"},
		PullRequest: &connector.PullRequest{
			Number: 1435,
			State:  "OPEN",
			Labels: []string{"Ready to Merge"},
		},
	}
	tests := []struct {
		name     string
		tracker  connector.Issue
		hydrated *connector.Issue
		reason   string
	}{
		{
			name: "board state changed",
			tracker: func() connector.Issue {
				issue := cloneIssue(base)
				issue.State = "Blocked"
				return issue
			}(),
			reason: mergeRevocationStateChanged,
		},
		{
			name: "approval label removed",
			tracker: func() connector.Issue {
				issue := cloneIssue(base)
				issue.Labels = []string{"bug"}
				return issue
			}(),
			reason: mergeRevocationApprovalLabelRemoved,
		},
		{
			name:    "pull request converted to draft",
			tracker: cloneIssue(base),
			hydrated: func() *connector.Issue {
				issue := cloneIssue(base)
				issue.PullRequest.Draft = true
				return &issue
			}(),
			reason: mergeRevocationDraftPullRequest,
		},
		{
			name:    "CI trigger label removed from pull request",
			tracker: cloneIssue(base),
			hydrated: func() *connector.Issue {
				issue := cloneIssue(base)
				issue.PullRequest.Labels = []string{}
				return &issue
			}(),
			reason: mergeRevocationCITriggerLabelRemoved,
		},
		{
			name:    "pull request closed",
			tracker: cloneIssue(base),
			hydrated: func() *connector.Issue {
				issue := cloneIssue(base)
				issue.PullRequest.State = "CLOSED"
				return &issue
			}(),
			reason: mergeRevocationPullRequestNotOpen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			runCtx, stop := context.WithCancelCause(context.Background())
			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				AutoPromote: AutoPromoteConfig{Gate: gate.Config{
					Kind:           gate.KindHumanReview,
					ApprovalLabel:  "Ready to Merge",
					CITriggerLabel: "Ready to Merge",
				}},
				ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates: []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			state.Running[base.ID] = Running{Issue: cloneIssue(base), stop: stop}
			state.Claimed[base.ID] = Claimed{Issue: cloneIssue(base)}
			tracker := &runningStateConnector{issues: []connector.Issue{tt.tracker}, hydratedIssue: tt.hydrated}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.reconcileRunningIssues(t.Context(), &state, now)

			select {
			case <-runCtx.Done():
			default:
				t.Fatal("merge worker context remains active after eligibility was revoked")
			}
			if !errors.Is(context.Cause(runCtx), runpkg.ErrMergeRevoked) {
				t.Fatalf("context cause = %v, want ErrMergeRevoked", context.Cause(runCtx))
			}
			if got := orch.pendingMergeRevocations[base.ID].reason; got != tt.reason {
				t.Fatalf("revocation reason = %q, want %q", got, tt.reason)
			}
		})
	}
}

func TestShouldReconcileRunningIssuesUsesPollIntervalForMergeWorkers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 10, 30, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{PollInterval: time.Minute})
	orch := &Orchestrator{cfg: cfg}
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{name: "merge worker", state: "Merging", want: true},
		{name: "implementation worker", state: "In Progress", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newState(cfg)
			state.LastRunningReconcileAt = now.Add(-90 * time.Second)
			issue := connector.Issue{ID: "issue-interval", State: tt.state}
			state.Running[issue.ID] = Running{Issue: issue}
			if got := orch.shouldReconcileRunningIssues(&state, now); got != tt.want {
				t.Fatalf("shouldReconcileRunningIssues() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestTickReapsTerminalRunningIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-10 * time.Minute)
	terminalAt := now.Add(-30 * time.Second)
	issue := connector.Issue{
		ID:         "issue-cancelled",
		Identifier: "digitaldrywood/detent#356",
		Title:      "Cancelled session",
		State:      "In Progress",
		URL:        "https://github.com/digitaldrywood/detent/issues/356",
	}
	cancelled := cloneIssue(issue)
	cancelled.State = "Cancelled"
	cancelled.StageUpdatedAt = &terminalAt

	project := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	slot, ok, err := gate.TryAcquire(context.Background(), project, scheduler.SlotRequest{State: issue.State}, startedAt)
	if err != nil {
		t.Fatalf("TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("TryAcquire() ok = false, want true")
	}

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		Project:             project,
		ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled", "Failed"},
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		StartedAt:  startedAt,
		Tokens:     TokenTotals{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, RuntimeSeconds: 90},
		globalSlot: slot,
		cancel:     cancel,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: startedAt, Owner: "worker-1"}

	tracker := &runningStateConnector{issues: []connector.Issue{cancelled}}
	orch := &Orchestrator{
		cfg:                cfg,
		connector:          tracker,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		globalDispatchGate: gate,
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after terminal reconciliation", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after terminal reconciliation", issue.ID)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after terminal reconciliation", issue.ID)
	}
	if completed.FinalState != "Cancelled" {
		t.Fatalf("Completed[%q].FinalState = %q, want Cancelled", issue.ID, completed.FinalState)
	}
	if !completed.CompletedAt.Equal(terminalAt) {
		t.Fatalf("Completed[%q].CompletedAt = %v, want %v", issue.ID, completed.CompletedAt, terminalAt)
	}
	if completed.Tokens.TotalTokens != 15 {
		t.Fatalf("Completed[%q].Tokens.TotalTokens = %d, want 15", issue.ID, completed.Tokens.TotalTokens)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}

	nextSlot, ok, err := gate.TryAcquire(context.Background(), project, scheduler.SlotRequest{State: "Todo"}, now)
	if err != nil {
		t.Fatalf("TryAcquire() after terminal reap error = %v", err)
	}
	if !ok {
		t.Fatal("TryAcquire() after terminal reap ok = false, want true")
	}
	if err := gate.Release(nextSlot); err != nil {
		t.Fatalf("Release() after terminal reap error = %v", err)
	}

	snapshot := state.Snapshot(now)
	if snapshot.Counts.Running != 0 || len(snapshot.Running) != 0 {
		t.Fatalf("snapshot running count = %d len = %d, want 0", snapshot.Counts.Running, len(snapshot.Running))
	}
	if snapshot.Counts.Completed != 1 || len(snapshot.Completed) != 1 {
		t.Fatalf("snapshot completed count = %d len = %d, want 1", snapshot.Counts.Completed, len(snapshot.Completed))
	}
}

func TestTickCancelledRunningIssueAuditsWorkspaceCleanupAndReleasesLease(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-10 * time.Minute)
	cancelledAt := now.Add(-30 * time.Second)
	issue := connector.Issue{
		ID:         "issue-cancelled-running",
		Identifier: "digitaldrywood/detent#586",
		Title:      "Cancellation audit",
		State:      "In Progress",
		URL:        "https://github.com/digitaldrywood/detent/issues/586",
	}
	cancelled := cloneIssue(issue)
	cancelled.State = "Cancelled"
	cancelled.StageUpdatedAt = &cancelledAt

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		Claiming: ClaimingConfig{
			Enabled:           true,
			OwnershipMode:     "field",
			Owner:             "detent-test",
			OwnerField:        "Owner",
			LeaseField:        "Lease",
			LeaseTTL:          time.Minute,
			HeartbeatInterval: time.Hour,
		},
		ActiveStates:                  []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Hour,
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:     cloneIssue(issue),
		StartedAt: startedAt,
		Tokens:    TokenTotals{TotalTokens: 15, RuntimeSeconds: 90},
		cancel:    cancel,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: startedAt, Owner: "detent-test"}

	tracker := &runningStateConnector{issuesByState: []connector.Issue{cancelled}}
	reaper := &cleanupSweepReaper{result: WorkspaceReapResult{Worktrees: 1, Branches: 1, Processes: 2}}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after cancellation cleanup", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after cancellation cleanup", issue.ID)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after cancellation cleanup", issue.ID)
	}
	if completed.FinalState != "Cancelled" {
		t.Fatalf("Completed[%q].FinalState = %q, want Cancelled", issue.ID, completed.FinalState)
	}
	if !completed.CompletedAt.Equal(cancelledAt) {
		t.Fatalf("Completed[%q].CompletedAt = %v, want %v", issue.ID, completed.CompletedAt, cancelledAt)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}
	if !tracker.hasSetField(issue.ID, "Lease", "") {
		t.Fatalf("SetField(%q, Lease, empty) not recorded; calls = %#v", issue.ID, tracker.setFieldCalls)
	}
	if len(reaper.issues) != 1 || reaper.issues[0].ID != issue.ID {
		t.Fatalf("reaped issues = %#v, want cancelled issue", reaper.issues)
	}

	event, ok := recentStateEvent(state, "workspace_reap_succeeded")
	if !ok {
		t.Fatalf("RecentEvents = %#v, want workspace_reap_succeeded", state.RecentEvents)
	}
	for _, want := range []string{
		"digitaldrywood/detent#586",
		"reason=cancelled",
		"worktrees=1",
		"branches=1",
		"processes=2",
	} {
		if !strings.Contains(event.Message, want) {
			t.Fatalf("cleanup event message = %q, want %q", event.Message, want)
		}
	}
	snapshot := state.Snapshot(now)
	if len(snapshot.Events) != 1 || snapshot.Events[0].Event != "workspace_reap_succeeded" {
		t.Fatalf("snapshot Events = %#v, want cleanup success event", snapshot.Events)
	}
}

func TestTickCancelledNonRunningIssueReapsWorkspaceEvenBeforeNextSweep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 12, 30, 0, 0, time.UTC)
	cancelled := connector.Issue{
		ID:         "issue-cancelled-non-running",
		Identifier: "digitaldrywood/detent#587",
		Title:      "Cancelled non-running work",
		State:      "Cancelled",
	}
	cfg := normalizeConfig(Config{
		PollInterval:                  time.Minute,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Hour,
	})
	state := newState(cfg)
	state.LastWorkspaceCleanupAt = now.Add(-time.Minute)

	tracker := &runningStateConnector{issuesByState: []connector.Issue{cancelled}}
	reaper := &cleanupSweepReaper{result: WorkspaceReapResult{Worktrees: 1}}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if len(reaper.issues) != 1 || reaper.issues[0].ID != cancelled.ID {
		t.Fatalf("reaped issues = %#v, want cancelled non-running issue before next sweep", reaper.issues)
	}
	if event, ok := recentStateEvent(state, "workspace_reap_succeeded"); !ok || !strings.Contains(event.Message, "digitaldrywood/detent#587") {
		t.Fatalf("RecentEvents = %#v, want cancelled cleanup success event", state.RecentEvents)
	}
}

func TestTickWorkspaceCleanupFailureRecordsDiagnosticEvent(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC)
	cancelled := connector.Issue{
		ID:         "issue-cancelled-failure",
		Identifier: "digitaldrywood/detent#588",
		Title:      "Cancelled cleanup failure",
		State:      "Cancelled",
	}
	cfg := normalizeConfig(Config{
		PollInterval:                  time.Minute,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates:                []string{"Human Review"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Hour,
	})
	state := newState(cfg)
	tracker := &runningStateConnector{issuesByState: []connector.Issue{cancelled}}
	workspacePath := "/tmp/detent-workspaces/digitaldrywood_detent_588"
	remediation := "chmod workspace cache directories and rerun cleanup"
	reaper := &cleanupSweepReaper{err: cleanupDiagnosticError{
		message:     "remove worktree: permission denied",
		path:        workspacePath,
		remediation: remediation,
	}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.ReapedWorkspaces[cancelled.ID]; ok {
		t.Fatalf("ReapedWorkspaces[%q] present after failed cleanup", cancelled.ID)
	}
	event, ok := recentStateEvent(state, "workspace_reap_failed")
	if !ok {
		t.Fatalf("RecentEvents = %#v, want workspace_reap_failed", state.RecentEvents)
	}
	for _, want := range []string{"digitaldrywood/detent#588", "reason=cancelled", "permission denied"} {
		if !strings.Contains(event.Message, want) {
			t.Fatalf("cleanup failure event message = %q, want %q", event.Message, want)
		}
	}
	for _, want := range []string{workspacePath, remediation} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("cleanup failure logs = %q, want %q", logs.String(), want)
		}
	}
}

func TestTickMarksClosedCompletedRunningIssueDoneBeforeReaping(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 13, 43, 15, 0, time.UTC)
	startedAt := now.Add(-15 * time.Minute)
	closedAt := now.Add(-46 * time.Second)
	issue := connector.Issue{
		ID:         "issue-closed-completed",
		Identifier: "digitaldrywood/detent#487",
		Title:      "Windows package managers",
		State:      "Merging",
		URL:        "https://github.com/digitaldrywood/detent/issues/487",
	}
	closed := cloneIssue(issue)
	closed.State = "Done"
	closed.Closed = true
	closed.ClosedReason = "completed"
	closed.StageUpdatedAt = &closedAt

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:     cloneIssue(issue),
		StartedAt: startedAt,
		Tokens:    TokenTotals{TotalTokens: 42, RuntimeSeconds: 90},
		cancel:    cancel,
	}

	tracker := &runningStateConnector{issues: []connector.Issue{closed}}
	reaper := &cleanupSweepReaper{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []statusUpdate{{issueID: issue.ID, state: "Done"}}; !slices.Equal(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing", issue.ID)
	}
	if completed.FinalState != "Done" || completed.Issue.State != "Done" {
		t.Fatalf("completed state = (%q, %q), want Done/Done", completed.Issue.State, completed.FinalState)
	}
	if len(reaper.issues) != 1 || reaper.issues[0].State != "Done" {
		t.Fatalf("reaped issues = %#v, want Done issue", reaper.issues)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}
}

func TestTickCompletesTerminalRunningIssueDuringWorkspaceCleanupSweep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 15, 15, 0, 0, time.UTC)
	startedAt := now.Add(-12 * time.Minute)
	terminalAt := now.Add(-30 * time.Second)
	prior := connector.Issue{
		ID:         "issue-merged",
		Identifier: "digitaldrywood/detent#453",
		Title:      "Release snapshot",
		State:      "Merging",
		URL:        "https://github.com/digitaldrywood/detent/issues/453",
	}
	done := cloneIssue(prior)
	done.State = "Done"
	done.StageUpdatedAt = &terminalAt

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:                  time.Minute,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		ObservedStates:                []string{"Human Review", "Merging"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Hour,
	})
	state := newState(cfg)
	state.LastRunningReconcileAt = now.Add(-time.Second)
	state.Running[prior.ID] = Running{
		Issue:       cloneIssue(prior),
		StartedAt:   startedAt,
		LastMessage: "GoReleaser Snapshot remains in progress; continuing to wait.",
		Tokens:      TokenTotals{TotalTokens: 42, RuntimeSeconds: 90},
		cancel:      cancel,
	}
	state.Claimed[prior.ID] = Claimed{Issue: cloneIssue(prior), ClaimedAt: startedAt, Owner: "worker-1"}

	tracker := &runningStateConnector{issuesByState: []connector.Issue{done}}
	reaper := &cleanupSweepReaper{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.Running[prior.ID]; ok {
		t.Fatalf("Running[%q] present after terminal cleanup sweep", prior.ID)
	}
	if _, ok := state.Claimed[prior.ID]; ok {
		t.Fatalf("Claimed[%q] present after terminal cleanup sweep", prior.ID)
	}
	completed, ok := state.Completed[prior.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after terminal cleanup sweep", prior.ID)
	}
	if completed.Issue.State != "Done" || completed.FinalState != "Done" {
		t.Fatalf("Completed[%q] state = (%q, %q), want Done/Done", prior.ID, completed.Issue.State, completed.FinalState)
	}
	if !completed.CompletedAt.Equal(terminalAt) {
		t.Fatalf("Completed[%q].CompletedAt = %v, want %v", prior.ID, completed.CompletedAt, terminalAt)
	}
	if completed.Tokens.TotalTokens != 42 {
		t.Fatalf("Completed[%q].Tokens.TotalTokens = %d, want 42", prior.ID, completed.Tokens.TotalTokens)
	}
	if !slices.Equal(tracker.requestedIDs, []string{prior.ID}) {
		t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want cleanup verification for %q", tracker.requestedIDs, prior.ID)
	}
	if len(reaper.issues) != 1 || reaper.issues[0].ID != prior.ID || reaper.issues[0].State != "Done" {
		t.Fatalf("reaped issues = %#v, want terminal Done issue", reaper.issues)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}

	snapshot := state.Snapshot(now.Add(time.Second))
	if !snapshot.GeneratedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("snapshot GeneratedAt = %v, want fresh publish time", snapshot.GeneratedAt)
	}
	if snapshot.Counts.Running != 0 || len(snapshot.Running) != 0 {
		t.Fatalf("snapshot running count = %d len = %d, want 0", snapshot.Counts.Running, len(snapshot.Running))
	}
	if snapshot.Counts.Completed != 1 || len(snapshot.Completed) != 1 || snapshot.Completed[0].State != "Done" {
		t.Fatalf("snapshot completed = %#v, want terminal Done row", snapshot.Completed)
	}
}

func TestTerminalCompletedAtUsesTerminalConditionTimestamp(t *testing.T) {
	t.Parallel()

	stageUpdatedAt := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fallback := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	terminalStates := normalizeConfig(Config{TerminalStates: []string{"Done", "Cancelled"}}).TerminalStates

	tests := []struct {
		name  string
		issue connector.Issue
		want  time.Time
	}{
		{
			name: "status terminal uses stage update time",
			issue: connector.Issue{
				State:          "Cancelled",
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
			},
			want: stageUpdatedAt,
		},
		{
			name: "closed active issue uses issue update time",
			issue: connector.Issue{
				State:          "In Progress",
				Closed:         true,
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
			},
			want: updatedAt,
		},
		{
			name: "merged active pull request uses issue update time",
			issue: connector.Issue{
				State:          "Merging",
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
				PullRequest:    &connector.PullRequest{State: "MERGED"},
			},
			want: updatedAt,
		},
		{
			name: "missing tracker timestamps uses fallback",
			issue: connector.Issue{
				State: "Cancelled",
			},
			want: fallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := terminalCompletedAt(tt.issue, terminalStates, fallback)
			if !got.Equal(tt.want) {
				t.Fatalf("terminalCompletedAt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeIssueTrackerFieldsDistinguishesMissingAndEmptyMetadata(t *testing.T) {
	t.Parallel()

	current := connector.Issue{
		ID:        "issue-running",
		Assignees: []string{"worker-1"},
		Fields:    map[string]string{"Status": "Todo"},
	}

	tests := []struct {
		name          string
		refreshed     connector.Issue
		wantAssignees []string
		wantFields    map[string]string
	}{
		{
			name:          "missing metadata preserves current values",
			refreshed:     connector.Issue{ID: current.ID},
			wantAssignees: []string{"worker-1"},
			wantFields:    map[string]string{"Status": "Todo"},
		},
		{
			name:          "explicit empty metadata clears current values",
			refreshed:     connector.Issue{ID: current.ID, Assignees: []string{}, Fields: map[string]string{}},
			wantAssignees: []string{},
			wantFields:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mergeIssueTrackerFields(current, tt.refreshed)
			if !slices.Equal(got.Assignees, tt.wantAssignees) {
				t.Fatalf("Assignees = %#v, want %#v", got.Assignees, tt.wantAssignees)
			}
			if len(got.Fields) != len(tt.wantFields) {
				t.Fatalf("Fields = %#v, want %#v", got.Fields, tt.wantFields)
			}
			for key, value := range tt.wantFields {
				if got.Fields[key] != value {
					t.Fatalf("Fields[%q] = %q, want %q", key, got.Fields[key], value)
				}
			}
		})
	}
}

func TestMergeIssueTrackerFieldsUpdatesMappedPriorityName(t *testing.T) {
	t.Parallel()

	current := connector.Issue{Priority: reconcilePriorityPointer(1), PriorityName: "P0"}
	tests := []struct {
		name         string
		refreshed    connector.Issue
		wantPriority *int
		wantName     string
	}{
		{name: "missing priority preserves current", wantPriority: reconcilePriorityPointer(1), wantName: "P0"},
		{name: "mapped priority updates rank and name", refreshed: connector.Issue{Priority: reconcilePriorityPointer(2), PriorityName: "P1"}, wantPriority: reconcilePriorityPointer(2), wantName: "P1"},
		{name: "unmapped option clears rank", refreshed: connector.Issue{PriorityName: "No priority"}, wantName: "No priority"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mergeIssueTrackerFields(current, tt.refreshed)
			if (got.Priority == nil) != (tt.wantPriority == nil) {
				t.Fatalf("Priority = %#v, want %#v", got.Priority, tt.wantPriority)
			}
			if got.Priority != nil && *got.Priority != *tt.wantPriority {
				t.Fatalf("Priority = %d, want %d", *got.Priority, *tt.wantPriority)
			}
			if got.PriorityName != tt.wantName {
				t.Fatalf("PriorityName = %q, want %q", got.PriorityName, tt.wantName)
			}
		})
	}
}

func reconcilePriorityPointer(value int) *int {
	return &value
}

type runningStateConnector struct {
	issues        []connector.Issue
	issuesByState []connector.Issue
	hydratedIssue *connector.Issue
	err           error
	requestedIDs  []string
	updates       []statusUpdate
	setFieldCalls []reconcileSetFieldCall
}

func (c *runningStateConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	if c.hydratedIssue == nil {
		return cloneIssue(issue), nil
	}
	return cloneIssue(*c.hydratedIssue), nil
}

func (c *runningStateConnector) Name() string {
	return "running-state"
}

func (c *runningStateConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *runningStateConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	return issuesInStates(c.issuesByState, states), nil
}

func (c *runningStateConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	c.requestedIDs = append([]string(nil), ids...)
	if c.err != nil {
		return nil, c.err
	}
	return cloneIssues(c.issues), nil
}

func (c *runningStateConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *runningStateConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, statusUpdate{issueID: issueID, state: state})
	return nil
}

func (c *runningStateConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *runningStateConnector) SetField(_ context.Context, issueID string, field string, value string) error {
	c.setFieldCalls = append(c.setFieldCalls, reconcileSetFieldCall{issueID: issueID, field: field, value: value})
	return nil
}

func (c *runningStateConnector) hasSetField(issueID string, field string, value string) bool {
	for _, call := range c.setFieldCalls {
		if call.issueID == issueID && call.field == field && call.value == value {
			return true
		}
	}
	return false
}

type reconcileSetFieldCall struct {
	issueID string
	field   string
	value   string
}

type cleanupSweepReaper struct {
	result WorkspaceReapResult
	err    error
	issues []connector.Issue
}

func (r *cleanupSweepReaper) ReapWorkspace(_ context.Context, issue connector.Issue) (WorkspaceReapResult, error) {
	r.issues = append(r.issues, cloneIssue(issue))
	return r.result, r.err
}

type cleanupDiagnosticError struct {
	message     string
	path        string
	remediation string
}

func (e cleanupDiagnosticError) Error() string {
	return e.message
}

func (e cleanupDiagnosticError) WorkspacePath() string {
	return e.path
}

func (e cleanupDiagnosticError) Remediation() string {
	return e.remediation
}

func recentStateEvent(state State, event string) (telemetryEvent, bool) {
	for _, candidate := range state.RecentEvents {
		if candidate.Event == event {
			return telemetryEvent{Event: candidate.Event, Message: candidate.Message}, true
		}
	}
	return telemetryEvent{}, false
}

type telemetryEvent struct {
	Event   string
	Message string
}
