package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestMergeRevocationCompletionReleasesAttemptAndCapacity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 11, 0, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:           "issue-revoked-capacity",
		Identifier:   "digitaldrywood/detent#1434",
		State:        "Merging",
		PRRepository: "digitaldrywood/detent",
		PullRequest: &connector.PullRequest{
			Number: 1435,
			State:  "OPEN",
		},
	}
	revoked := cloneIssue(issue)
	revoked.State = "Blocked"
	project := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
	dispatchGate := scheduler.NewGlobalDispatchGate(scheduler.NewRoundRobin(scheduler.Config{Capacity: 1}))
	slot, ok, err := dispatchGate.TryAcquire(t.Context(), project, scheduler.SlotRequest{State: "Merging"}, now)
	if err != nil || !ok {
		t.Fatalf("TryAcquire() = %#v, %v, want acquired slot", slot, err)
	}
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		Project:             project,
		ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	attempts := &recordingWorkAttemptStore{}
	tracker := &runningStateConnector{issues: []connector.Issue{revoked}}
	orch := &Orchestrator{
		cfg:                cfg,
		connector:          tracker,
		workAttempts:       attempts,
		globalDispatchGate: dispatchGate,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	runCtx, stop := context.WithCancelCause(context.Background())
	state.Running[issue.ID] = Running{
		Issue:         cloneIssue(issue),
		Attempt:       2,
		WorkAttemptID: 42,
		Mode:          runpkg.RunModeMerge,
		StartedAt:     now.Add(-time.Hour),
		globalSlot:    slot,
		stop:          stop,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Hour)}
	state.Retry[issue.ID] = Retry{Issue: cloneIssue(issue), Attempt: 3, DueAt: now.Add(time.Hour)}

	orch.reconcileRunningIssues(t.Context(), &state, now)
	if !errors.Is(context.Cause(runCtx), runpkg.ErrMergeRevoked) {
		t.Fatalf("context cause = %v, want ErrMergeRevoked", context.Cause(runCtx))
	}
	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID:     issue.ID,
		CompletedAt: now.Add(time.Second),
		Err:         runpkg.ErrMergeRevoked,
	})

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after merge revocation", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after merge revocation", issue.ID)
	}
	if _, ok := state.Retry[issue.ID]; ok {
		t.Fatalf("Retry[%q] present after merge revocation", issue.ID)
	}
	if len(attempts.completions) != 1 {
		t.Fatalf("work attempt completions = %#v, want one", attempts.completions)
	}
	completion := attempts.completions[0]
	if completion.TerminalState != store.WorkAttemptTerminalMergeRevoked || completion.Phase != "merge_revoked" {
		t.Fatalf("work attempt completion = %#v, want merge_revoked terminal state", completion)
	}
	if len(tracker.updates) != 0 {
		t.Fatalf("tracker updates = %#v, want operator-selected Blocked state preserved", tracker.updates)
	}
	next, ok, err := dispatchGate.TryAcquire(t.Context(), project, scheduler.SlotRequest{State: "Todo"}, now.Add(2*time.Second))
	if err != nil || !ok {
		t.Fatalf("TryAcquire() after revocation = %#v, %v, want released capacity", next, err)
	}
}

func TestProgrammaticMergeRechecksEligibilityBeforeMerge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	issue := connector.Issue{
		ID:           "issue-final-recheck",
		Identifier:   "digitaldrywood/detent#1434",
		State:        "Merging",
		PRRepository: "digitaldrywood/detent",
		PullRequest: &connector.PullRequest{
			Number:         1435,
			URL:            "https://github.test/digitaldrywood/detent/pull/1435",
			State:          "OPEN",
			MergeableState: "clean",
			CIStatus:       "success",
			HeadSHA:        "revoked-head",
			Labels:         []string{"Ready to Merge"},
		},
	}
	revoked := cloneIssue(issue)
	revoked.PullRequest.Labels = []string{}
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{revoked}}
	mergeConnector := &autoPromoteTickMergeConnector{autoPromoteTickConnector: tracker}
	cfg := normalizeConfig(Config{
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Human Review",
			Gate: gate.Config{
				Kind:           gate.KindCommand,
				CITriggerLabel: "Ready to Merge",
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	attempts := &recordingWorkAttemptStore{}
	orch := &Orchestrator{
		cfg:                     cfg,
		connector:               mergeConnector,
		workAttempts:            attempts,
		pendingMergeRevocations: map[string]mergeRevocation{},
		logger:                  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:         cloneIssue(issue),
		Attempt:       1,
		WorkAttemptID: 43,
		Mode:          runpkg.RunModeMerge,
		StartedAt:     now.Add(-time.Minute),
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}

	orch.handleRunResult(t.Context(), &state, runpkg.Completion{
		IssueID: issue.ID,
		Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
		Result: runpkg.RunResult{
			FinalState:  runpkg.FinalStateCompleted,
			Output:      runpkg.RunOutputMergeFastPathClean,
			TurnStarted: true,
		},
		CompletedAt: now,
	})

	if len(mergeConnector.merges) != 0 {
		t.Fatalf("programmatic merges = %#v, want none after eligibility revocation", mergeConnector.merges)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Human Review" {
		t.Fatalf("tracker updates = %#v, want Human Review demotion", tracker.updates)
	}
	if len(attempts.completions) != 1 || attempts.completions[0].TerminalState != store.WorkAttemptTerminalMergeRevoked {
		t.Fatalf("work attempt completions = %#v, want merge_revoked", attempts.completions)
	}
}

func TestDraftMergeRevocationUsesConfiguredSourceState(t *testing.T) {
	t.Parallel()

	cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{SourceState: "Review"}})
	issue := connector.Issue{
		ID:    "issue-custom-review-lane",
		State: "Merging",
		PullRequest: &connector.PullRequest{
			State: "OPEN",
			Draft: true,
		},
	}

	revocation, revoked := mergeRevocationForIssue(issue, cfg, true)
	if !revoked {
		t.Fatal("mergeRevocationForIssue() did not revoke a draft pull request")
	}
	if revocation.targetState != "Review" {
		t.Fatalf("target state = %q, want configured source state Review", revocation.targetState)
	}
}

func TestMergeRevocationCommentsDeduplicateReasonAndHeadSHA(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := &mergeRevocationCommentConnector{now: now}
	orch := &Orchestrator{
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       func() time.Time { return now },
	}
	revocation := mergeRevocation{
		issue: connector.Issue{
			ID:    "issue-comment-dedup",
			State: "Merging",
			PullRequest: &connector.PullRequest{
				HeadSHA: "same-head",
			},
		},
		reason:      mergeRevocationDraftPullRequest,
		targetState: "In Progress",
	}

	orch.commentMergeRevocation(t.Context(), &State{}, revocation, now)
	orch = &Orchestrator{
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:       func() time.Time { return now },
	}
	for range 19 {
		orch.commentMergeRevocation(t.Context(), &State{}, revocation, now)
	}

	if got := len(tracker.comments); got != 1 {
		t.Fatalf("comments = %d, want 1", got)
	}
	body := tracker.comments[0].Body
	for _, want := range []string{
		"- reason: " + mergeRevocationDraftPullRequest,
		"- head_sha: same-head",
		mergeRevocationCommentSignature(revocation),
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment body = %q, want %q", body, want)
		}
	}
}

func TestMergeRevocationCommentBudgetWarnsAndEscalatesOnce(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := &mergeRevocationCommentConnector{now: now}
	for index := range mergeRevocationCommentLimit {
		createdAt := now.Add(-time.Duration(index) * time.Minute)
		revocation := mergeRevocation{
			issue: connector.Issue{
				ID: "issue-comment-budget",
				PullRequest: &connector.PullRequest{
					HeadSHA: fmt.Sprintf("prior-head-%d", index),
				},
			},
			reason: mergeRevocationDraftPullRequest,
		}
		tracker.comments = append(tracker.comments, connector.IssueComment{
			Body:      mergeRevocationCommentSignature(revocation),
			CreatedAt: &createdAt,
		})
	}
	var logs bytes.Buffer
	orch := &Orchestrator{
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		now:       func() time.Time { return now },
	}
	revocation := mergeRevocation{
		issue: connector.Issue{
			ID:    "issue-comment-budget",
			State: "Merging",
			PullRequest: &connector.PullRequest{
				HeadSHA: "new-head",
			},
		},
		reason: mergeRevocationDraftPullRequest,
	}

	orch.commentMergeRevocation(t.Context(), &State{}, revocation, now)
	orch.commentMergeRevocation(t.Context(), &State{}, revocation, now.Add(time.Minute))

	if got := len(tracker.comments); got != mergeRevocationCommentLimit {
		t.Fatalf("comments = %d, want budget limit %d", got, mergeRevocationCommentLimit)
	}
	if got, want := tracker.updates, []string{autoPromoteSourceState}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if got := strings.Count(logs.String(), "merge revocation comment budget exhausted"); got != 1 {
		t.Fatalf("budget warnings = %d, want 1: %s", got, logs.String())
	}
}

func TestMergeRevocationCommentResourceExhaustionEscalates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	tracker := &mergeRevocationCommentConnector{
		now:        now,
		commentErr: fmt.Errorf("create github comment: %w", connector.ErrResourceExhausted),
	}
	var logs bytes.Buffer
	orch := &Orchestrator{
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		now:       func() time.Time { return now },
	}
	revocation := mergeRevocation{
		issue: connector.Issue{
			ID:    "issue-comment-cap",
			State: "Merging",
			PullRequest: &connector.PullRequest{
				HeadSHA: "capped-head",
			},
		},
		reason: mergeRevocationDraftPullRequest,
	}

	orch.commentMergeRevocation(t.Context(), &State{}, revocation, now)
	orch.commentMergeRevocation(t.Context(), &State{}, revocation, now.Add(time.Minute))

	if tracker.commentAttempts != 1 {
		t.Fatalf("comment attempts = %d, want 1", tracker.commentAttempts)
	}
	if got, want := tracker.updates, []string{autoPromoteSourceState}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if !strings.Contains(logs.String(), "comment resource exhausted") {
		t.Fatalf("logs = %q, want resource exhaustion", logs.String())
	}
}

type mergeRevocationCommentConnector struct {
	now             time.Time
	comments        []connector.IssueComment
	updates         []string
	commentErr      error
	commentAttempts int
}

func (c *mergeRevocationCommentConnector) Name() string {
	return "merge-revocation-comment"
}

func (c *mergeRevocationCommentConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, nil
}

func (c *mergeRevocationCommentConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *mergeRevocationCommentConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, nil
}

func (c *mergeRevocationCommentConnector) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	return append([]connector.IssueComment(nil), c.comments...), nil
}

func (c *mergeRevocationCommentConnector) CreateComment(_ context.Context, _ string, body string) error {
	c.commentAttempts++
	if c.commentErr != nil {
		return c.commentErr
	}
	createdAt := c.now
	c.comments = append(c.comments, connector.IssueComment{
		Body:      body,
		CreatedAt: &createdAt,
	})
	return nil
}

func (c *mergeRevocationCommentConnector) UpdateIssueState(_ context.Context, _ string, state string) error {
	c.updates = append(c.updates, state)
	return nil
}

func (c *mergeRevocationCommentConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *mergeRevocationCommentConnector) SetField(context.Context, string, string, string) error {
	return nil
}
