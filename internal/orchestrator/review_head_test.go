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
)

func TestAutoPromoteReviewAtHead(t *testing.T) {
	for _, tt := range []struct {
		name           string
		required       bool
		review, latest string
		threads        int
		want           AutoPromoteReason
	}{
		{"opted out stale", false, "", "COMMENTED", 0, AutoPromoteReasonReady},
		{"opted out absent", false, "", "", 0, AutoPromoteReasonReady},
		{"opted out unresolved", false, "", "COMMENTED", 1, AutoPromoteReasonUnresolvedReviewThreads},
		{"required stale", true, "", "COMMENTED", 0, AutoPromoteReasonCodexReviewMissing},
		{"required absent", true, "", "", 0, AutoPromoteReasonCodexReviewMissing},
		{"required current", true, "COMMENTED", "COMMENTED", 0, AutoPromoteReasonReady},
		{"current unresolved", true, "COMMENTED", "COMMENTED", 1, AutoPromoteReasonUnresolvedReviewThreads},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := autoPromoteTestIssue("review-head", nil)
			issue.PullRequest = &connector.PullRequest{State: "OPEN", HeadSHA: "ced1be0", CIStatus: "success", CodexReviewState: tt.review, LatestCodexReviewState: tt.latest, LatestCodexReviewCommitSHA: "156b300", UnresolvedReviewThreads: make([]connector.PullRequestReviewThread, tt.threads)}
			summary := AutoPromoteSummaryFromIssue(issue)
			cfg := AutoPromoteConfig{Enabled: true, Gate: gate.Config{Kind: gate.KindCommand, RequireAutomatedReview: new(tt.required)}}
			got := EvaluateAutoPromote(issue, summary, cfg, time.Now())
			if got.Reason != tt.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tt.want)
			}
			diagnostic := requiredGateFromSummary(issue, summary, cfg, time.Now())
			if (diagnostic.State == "passed") != (tt.want == AutoPromoteReasonReady) {
				t.Fatalf("required gate = %+v", diagnostic)
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
		required     bool
		want         bool
	}{
		{"required stale head", "", true, false},
		{"opted out stale head", "", false, true},
		{"opted out pending review", "PENDING", false, true},
		{"pending current review", "PENDING", true, false},
		{"completed current review", "COMMENTED", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			issue := autoPromoteTickIssue("merge-review", nil, &connector.PullRequest{Number: 42, URL: "https://github.test/digitaldrywood/detent/pull/42", State: "OPEN", HeadSHA: "ced1be0", CIStatus: "success", MergeableState: "clean", CodexReviewState: tt.review, LatestCodexReviewState: "COMMENTED", LatestCodexReviewCommitSHA: "156b300"})
			issue.State = "Merging"
			cfg := Config{AutoPromote: AutoPromoteConfig{Gate: gate.Config{RequireAutomatedReview: new(tt.required)}}}
			if got := nativeMergeQueueCandidate(issue, cfg); got != tt.want {
				t.Fatalf("native queue admission = %v, want %v", got, tt.want)
			}
			if got := mergeWorkerProgrammaticMergeReady(issue, cfg); got != tt.want {
				t.Fatalf("programmatic merge ready = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReviewPolicyDispatchDiagnostic(t *testing.T) {
	for _, required := range []bool{false, true} {
		mode := gate.AutomatedReviewOff
		if required {
			mode = gate.AutomatedReviewRequired
		}
		t.Run(mode, func(t *testing.T) {
			cfg := normalizeConfig(Config{AutoPromote: AutoPromoteConfig{Gate: gate.Config{RequireAutomatedReview: new(required)}}})
			o := &Orchestrator{cfg: cfg}
			state := newState(cfg)
			attrs := o.schedulerDecisionAttrs(&state, time.Now(), autoPromoteTestIssue("diagnostic", nil))
			for i := 0; i+1 < len(attrs); i += 2 {
				if attrs[i] == "gate_automated_review_mode" {
					if attrs[i+1] != mode {
						t.Fatalf("review mode = %v, want %s", attrs[i+1], mode)
					}
					return
				}
			}
			t.Fatal("dispatch diagnostic lacks review policy")
		})
	}
}
