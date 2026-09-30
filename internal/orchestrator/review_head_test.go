package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestAutoPromoteReviewAtHead(t *testing.T) {
	for _, tt := range []struct {
		name, review string
		threads      int
		expired      bool
		want         AutoPromoteReason
	}{
		{"stale review before deadline", "", 0, false, AutoPromoteReasonCodexReviewMissing},
		{"stale review after deadline", "", 0, true, AutoPromoteReasonReady},
		{"current review zero threads", "COMMENTED", 0, true, AutoPromoteReasonReady},
		{"current review unresolved thread", "COMMENTED", 1, true, AutoPromoteReasonUnresolvedReviewThreads},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := autoPromoteTestIssue("review-head", nil)
			issue.PullRequest = &connector.PullRequest{State: "OPEN", HeadSHA: "ced1be0", CIStatus: "success", CodexReviewState: tt.review, LatestCodexReviewState: "COMMENTED", LatestCodexReviewCommitSHA: "156b300", UnresolvedReviewThreads: make([]connector.PullRequestReviewThread, tt.threads)}
			summary := AutoPromoteSummaryFromIssue(issue)
			summary.AutomatedReviewWaitExpired = tt.expired
			got := EvaluateAutoPromote(issue, summary, AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}}, time.Now())
			if got.Reason != tt.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tt.want)
			}
		})
	}
}

type reviewHeadConnector struct {
	autoPromoteTickConnector
	readErr  error
	writeErr error
}

func (c *reviewHeadConnector) FetchPullRequestComments(context.Context, string, int) ([]connector.IssueComment, error) {
	if c.readErr != nil {
		return nil, c.readErr
	}
	var out []connector.IssueComment
	for _, comment := range c.prComments {
		out = append(out, connector.IssueComment{Body: comment.body})
	}
	return out, nil
}

func (c *reviewHeadConnector) CreatePullRequestComment(ctx context.Context, repository string, number int, body string) error {
	if c.writeErr != nil {
		return c.writeErr
	}
	return c.autoPromoteTickConnector.CreatePullRequestComment(ctx, repository, number, body)
}

func TestReviewHeadRequestAndWait(t *testing.T) {
	for _, tt := range []struct {
		name              string
		readErr, writeErr error
		wantRequests      int
	}{
		{name: "one request across repeated ticks", wantRequests: 1},
		{name: "read failure keeps waiting", readErr: errors.New("read unavailable")},
		{name: "write failure keeps waiting", writeErr: errors.New("write unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{PollInterval: time.Minute, MaxConcurrentAgents: 1, AutoPromote: AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(true)}}, ActiveStates: []string{"Todo", "In Progress", "Rework", "Merging"}, TerminalStates: []string{"Done", "Cancelled"}})
			issue := autoPromoteTickIssue("review-head", nil, &connector.PullRequest{Number: 42, URL: "https://github.test/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "ced1be0", CIStatus: "success", LatestCodexReviewState: "COMMENTED", LatestCodexReviewCommitSHA: "156b300"})
			tracker := &reviewHeadConnector{autoPromoteTickConnector: autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}, readErr: tt.readErr, writeErr: tt.writeErr}
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			now := time.Now()
			for i := range 2 {
				orch.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Duration(i)*time.Minute))
			}
			if len(tracker.updates) != 0 {
				t.Fatalf("stale head changed lane: %+v", tracker.updates)
			}
			if len(tracker.prComments) != tt.wantRequests {
				t.Fatalf("requests = %d, want %d", len(tracker.prComments), tt.wantRequests)
			}
			if tt.wantRequests > 0 && !strings.Contains(tracker.prComments[0].body, "@codex review") {
				t.Fatal("missing review trigger")
			}
			tracker.readErr, tracker.writeErr = nil, nil
			issue.PullRequest.HeadSHA = "next-head"
			orch.autoPromoteHumanReviewIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(2*time.Minute))
			if len(tracker.prComments) != tt.wantRequests+1 {
				t.Fatal("new head did not request review")
			}
		})
	}
}

func TestReviewHeadMergeAdmission(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, review string
		want         bool
	}{
		{"stale head", "", false},
		{"pending current review", "PENDING", false},
		{"completed current review", "COMMENTED", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := autoPromoteTickIssue("merge-review", nil, &connector.PullRequest{Number: 42, URL: "https://github.test/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "ced1be0", CIStatus: "success", MergeableState: "clean", CodexReviewState: tt.review, LatestCodexReviewState: "COMMENTED", LatestCodexReviewCommitSHA: "156b300"})
			issue.State = "Merging"
			if got := nativeMergeQueueCandidate(issue, Config{}); got != tt.want {
				t.Fatalf("native queue admission = %v, want %v", got, tt.want)
			}
			if got := mergeWorkerProgrammaticMergeReady(issue, Config{}); got != tt.want {
				t.Fatalf("programmatic merge ready = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReviewGateQueueAndRequest(t *testing.T) {
	for _, tt := range []struct {
		name             string
		gate             gate.Config
		reviewed         bool
		enqueue, request bool
	}{
		{"human pending", gate.Config{Kind: gate.KindHumanReview}, false, true, false},
		{"human quota reply", gate.Config{Kind: gate.KindHumanReview}, true, true, false},
		{"disabled", gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}, false, true, false},
		{"optional", gate.Config{Kind: gate.KindCommand, AutomatedReview: gate.AutomatedReviewOptional}, false, true, false},
		{"required pending", gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(true)}, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Gate: tt.gate}, ActiveStates: []string{"Merging"}})
			issue := nativeMergeQueueTestIssue(42, "success")
			issue.Labels = append(issue.Labels, "human-approved")
			issue.PullRequest.LatestCodexReviewState = "PENDING"
			if tt.reviewed {
				issue.PullRequest.CodexReviewState = "COMMENTED"
			}
			tracker := &nativeMergeQueueConnector{autoPromoteTickMergeConnector: &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker}
			state := newState(cfg)
			orch.delegateNativeMergeQueueIssues(t.Context(), &state, []connector.Issue{issue}, time.Now())
			if got := len(tracker.enqueued) > 0; got != tt.enqueue {
				t.Fatalf("enqueued = %v, want %v", got, tt.enqueue)
			}
			requests := &reviewHeadConnector{}
			orch.connector = requests
			orch.requestAutomatedReview(t.Context(), issue)
			if got := len(requests.prComments) > 0; got != tt.request {
				t.Fatalf("requested = %v, want %v", got, tt.request)
			}
		})
	}
}

func TestReviewGateMergeWorkerWithoutQueue(t *testing.T) {
	for _, tt := range []struct {
		name      string
		gate      gate.Config
		wantMerge bool
	}{
		{"human review stale bot evidence", gate.Config{Kind: gate.KindHumanReview}, true},
		{"disabled review stale bot evidence", gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(false)}, true},
		{"required review stale bot evidence", gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(true)}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Gate: tt.gate}, ActiveStates: []string{"Merging", "Rework"}, TerminalStates: []string{"Done", "Cancelled"}})
			issue := nativeMergeQueueTestIssue(42, "success")
			issue.Labels = append(issue.Labels, "human-approved")
			issue.PullRequest.LatestCodexReviewState = "COMMENTED"
			issue.PullRequest.LatestCodexReviewCommitSHA = "old-head"
			issue.PullRequest.CodexReviewState = ""
			issue.PullRequest.MergeableState = "clean"
			if got := mergeWorkerProgrammaticMergeReady(issue, cfg); got != tt.wantMerge {
				t.Fatalf("programmatic readiness = %v, want %v", got, tt.wantMerge)
			}
			tracker := &autoPromoteTickMergeConnector{autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}}
			orch := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := newState(cfg)
			now := time.Now()
			state.Running[issue.ID] = Running{Issue: cloneIssue(issue), Attempt: 1, StartedAt: now.Add(-time.Minute), Mode: runpkg.RunModeMerge}
			state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: now.Add(-time.Minute)}
			orch.handleRunResult(t.Context(), &state, runpkg.Completion{
				IssueID: issue.ID, CompletedAt: now,
				Request: runpkg.RunRequest{Mode: runpkg.RunModeMerge},
				Result:  runpkg.RunResult{FinalState: runpkg.FinalStateCompleted, Output: runpkg.RunOutputMergeFastPathClean},
			})
			if got := len(tracker.merges) == 1; got != tt.wantMerge {
				t.Fatalf("merged = %v, want %v; retry = %+v", got, tt.wantMerge, state.Retry[issue.ID])
			}
		})
	}
}
