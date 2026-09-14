package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

// nativeChangeReview is the change review surface a hub-native item on a
// project with no GitHub connector reports: no pull request can ever mirror
// the change, and the revision an attempt last recorded one for.
func nativeChangeReview(changeAtCurrentRevision bool) *connector.ChangeReview {
	review := &connector.ChangeReview{
		PullRequestsAvailable:   false,
		ChangeAtCurrentRevision: changeAtCurrentRevision,
		Revision:                2,
	}
	if changeAtCurrentRevision {
		review.ChangeRevision = 2
	}
	return review
}

// connectedChangeReview is the same surface on a project that does have a
// GitHub connector. A pull request could mirror the change, but nothing in a
// conversation-driven attempt opens one -- that is an explicit action of the
// merge lane (decisions section 18.6) -- so the item is judged on its change
// exactly as a connectorless one is.
func connectedChangeReview(changeAtCurrentRevision bool) *connector.ChangeReview {
	review := &connector.ChangeReview{
		Provider:                "github",
		PullRequestsAvailable:   true,
		ChangeAtCurrentRevision: changeAtCurrentRevision,
		Revision:                2,
	}
	if changeAtCurrentRevision {
		review.ChangeRevision = 2
	} else {
		review.ChangeRevision = 1
	}
	return review
}

// TestCompletedActiveIssueReadyForReview is the readiness rule. It used to be
// a pull request and nothing else, which no hub-native item can ever have, so
// a native project's completed item was never promoted (operations.md section
// 8, the seventh dogfood run).
func TestCompletedActiveIssueReadyForReview(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                          string
		issue                         connector.Issue
		requirePullRequest            bool
		operationalCompletionAccepted bool
		wantReady                     bool
		wantMissing                   string
	}{
		{
			name: "native item with no connector and a recorded change is ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.ChangeReview = nativeChangeReview(true)
				return issue
			}(),
			requirePullRequest: true,
			wantReady:          true,
		},
		{
			name: "native item with no connector and no recorded change is not ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.ChangeReview = nativeChangeReview(false)
				return issue
			}(),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingChange,
		},
		{
			name: "native item with a connector and an open pull request is ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "OPEN")
				issue.ChangeReview = connectedChangeReview(true)
				return issue
			}(),
			requirePullRequest: true,
			wantReady:          true,
		},
		{
			name: "native item with a connector and a change but no pull request is ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.ChangeReview = connectedChangeReview(true)
				return issue
			}(),
			requirePullRequest: true,
			wantReady:          true,
		},
		{
			name: "native item with a connector and a change below the item's revision is not ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.ChangeReview = connectedChangeReview(false)
				return issue
			}(),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingChange,
		},
		{
			name: "native item with a connector and a merged pull request is not ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "MERGED")
				issue.ChangeReview = connectedChangeReview(true)
				return issue
			}(),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingOpenPullRequest,
		},
		{
			name: "native item with a connector and a closed pull request is not ready",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "CLOSED")
				issue.ChangeReview = connectedChangeReview(true)
				return issue
			}(),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingOpenPullRequest,
		},
		{
			name:               "a github profile item with an open pull request is ready",
			issue:              completionTransitionIssue("In Progress", "OPEN"),
			requirePullRequest: true,
			wantReady:          true,
		},
		{
			name:               "a github profile item with no pull request is not ready",
			issue:              completionTransitionIssue("In Progress", ""),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingPullRequest,
		},
		{
			name:               "a github profile item with a closed pull request is not ready",
			issue:              completionTransitionIssue("In Progress", "CLOSED"),
			requirePullRequest: true,
			wantMissing:        completedReviewMissingOpenPullRequest,
		},
		{
			name:               "a gate that wants no pull request is ready without one",
			issue:              completionTransitionIssue("In Progress", ""),
			requirePullRequest: false,
			wantReady:          true,
		},
		{
			name: "an accepted operational completion is ready without any change",
			issue: func() connector.Issue {
				issue := completionTransitionIssue("In Progress", "")
				issue.ChangeReview = nativeChangeReview(false)
				return issue
			}(),
			requirePullRequest:            true,
			operationalCompletionAccepted: true,
			wantReady:                     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ready, missing := completedActiveIssueReadyForReview(
				test.issue,
				test.requirePullRequest,
				test.operationalCompletionAccepted,
			)
			if ready != test.wantReady || missing != test.wantMissing {
				t.Fatalf("completedActiveIssueReadyForReview() = (%t, %q), want (%t, %q)",
					ready, missing, test.wantReady, test.wantMissing)
			}
		})
	}
}

// changeReviewConnector is a workflowStateConnector that also reports the
// item's change review surface, the way the native Hub connector does. The
// issue the orchestrator holds was read when the attempt was dispatched, so
// the hydration is the only place the change the attempt produced can come
// from.
type changeReviewConnector struct {
	*workflowStateConnector
	review     *connector.ChangeReview
	err        error
	hydrations []string
}

func (c *changeReviewConnector) HydrateChangeReview(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrations = append(c.hydrations, strings.TrimSpace(issue.ID))
	if c.err != nil {
		return issue, c.err
	}
	hydrated := issue
	hydrated.ChangeReview = c.review
	return hydrated, nil
}

func newChangeReviewConnector(issue connector.Issue, review *connector.ChangeReview) *changeReviewConnector {
	return &changeReviewConnector{
		workflowStateConnector: &workflowStateConnector{
			autoPromoteTickConnector: &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}},
			states:                   threeStateWorkflow(),
		},
		review: review,
	}
}

func completedNativeTickConfig() Config {
	return normalizeConfig(Config{
		AutoPromote:    AutoPromoteConfig{Gate: gate.Config{Kind: gate.KindCommand}},
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done"},
	})
}

// TestTransitionCompletedActiveIssuesPromotesNativeItemOnItsOwnChange is the
// seventh dogfood run's scenario: a three-state native project with a command
// gate, an attempt that succeeded and posted its diff, and an item that carries
// no pull request because its project has no GitHub connector. Before the fix
// completedActiveReviewTargetState answered "" and the loop continued in
// silence, so the fallback to Done was never even asked for.
func TestTransitionCompletedActiveIssuesPromotesNativeItemOnItsOwnChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 13, 30, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "")
	tracker := newChangeReviewConnector(issue, nativeChangeReview(true))
	cfg := completedNativeTickConfig()
	orch := &Orchestrator{cfg: cfg, connector: tracker}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want the completed native issue", result.transitioned)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
		t.Fatalf("updates = %#v, want one move to Done", tracker.updates)
	}
	if len(tracker.hydrations) != 1 || tracker.hydrations[0] != issue.ID {
		t.Fatalf("change review hydrations = %#v, want one for %s", tracker.hydrations, issue.ID)
	}
}

// TestTransitionCompletedActiveIssuesReportsMissingNativeChange covers the
// other half: an attempt that succeeded without recording a change leaves the
// item where it is, and says which fact was missing. The silent continue is
// what hid the whole promotion for seven runs.
func TestTransitionCompletedActiveIssuesReportsMissingNativeChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 13, 30, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "")
	tracker := newChangeReviewConnector(issue, nativeChangeReview(false))
	cfg := completedNativeTickConfig()
	logs := &bytes.Buffer{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	for range 3 {
		if result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now); len(result.transitioned) != 0 {
			t.Fatalf("transitioned = %#v, want none: the item is not ready", result.transitioned)
		}
	}

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	if lines := strings.Count(logs.String(), "completed issue not ready for promotion"); lines != 1 {
		t.Fatalf("skip log lines = %d, want exactly one per item:\n%s", lines, logs.String())
	}
	if !strings.Contains(logs.String(), "missing="+completedReviewMissingChange) {
		t.Fatalf("skip log does not name the missing fact:\n%s", logs.String())
	}
}

// TestTransitionCompletedActiveIssuesPromotesConnectedItemOnItsOwnChange is
// the eighth dogfood run's scenario: the same native item on a project that
// does have a GitHub connector bound, whose attempt posted its diff and opened
// no pull request, because opening one is an explicit action of the merge lane
// (decisions section 18.6). The rule used to demand a pull request that would
// never exist, and the item sat In Progress forever.
func TestTransitionCompletedActiveIssuesPromotesConnectedItemOnItsOwnChange(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 13, 30, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "")
	tracker := newChangeReviewConnector(issue, connectedChangeReview(true))
	cfg := completedNativeTickConfig()
	logs := &bytes.Buffer{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	result := orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if _, ok := result.transitioned[issue.ID]; !ok {
		t.Fatalf("transitioned = %#v, want the completed item on a connector project", result.transitioned)
	}
	if len(tracker.updates) != 1 || tracker.updates[0].state != "Done" {
		t.Fatalf("updates = %#v, want one move to Done", tracker.updates)
	}
	if strings.Contains(logs.String(), "completed issue not ready for promotion") {
		t.Fatalf("the item was reported not ready:\n%s", logs.String())
	}
}

// TestTransitionCompletedActiveIssuesKeepsOpenPullRequestRule proves the half
// of the pull-request rule that stays: a pull request that was opened for the
// item still has to be open, so an item whose pull request merged or closed is
// left alone and the reason says so.
func TestTransitionCompletedActiveIssuesKeepsOpenPullRequestRule(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 12, 13, 30, 0, 0, time.UTC)
	issue := completionTransitionIssue("In Progress", "MERGED")
	tracker := newChangeReviewConnector(issue, connectedChangeReview(true))
	cfg := completedNativeTickConfig()
	logs := &bytes.Buffer{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	state := newState(cfg)
	state.Completed[issue.ID] = Completed{Issue: issue, CompletedAt: now.Add(-time.Minute), FinalState: FinalStateCompleted}

	orch.transitionCompletedActiveIssuesToReview(t.Context(), &state, []connector.Issue{issue}, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none: the item's pull request is not open", tracker.updates)
	}
	if !strings.Contains(logs.String(), "missing="+completedReviewMissingOpenPullRequest) {
		t.Fatalf("skip log does not name the pull request that is no longer open:\n%s", logs.String())
	}
}

// TestHydrateCompletedChangeReviewToleratesFailure keeps a tracker that cannot
// answer from changing the decision: the issue is left as it was read, the
// failure is logged, and the promotion falls back to the pull request rule.
func TestHydrateCompletedChangeReviewToleratesFailure(t *testing.T) {
	t.Parallel()

	issue := completionTransitionIssue("In Progress", "OPEN")
	tracker := newChangeReviewConnector(issue, nativeChangeReview(true))
	tracker.err = errors.New("hub unavailable")
	logs := &bytes.Buffer{}
	orch := &Orchestrator{
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}

	hydrated := orch.hydrateCompletedChangeReview(t.Context(), issue)

	if hydrated.ChangeReview != nil {
		t.Fatalf("change review = %#v, want the issue left as it was read", hydrated.ChangeReview)
	}
	if !strings.Contains(logs.String(), "read issue change review failed") {
		t.Fatalf("hydration failure was not logged:\n%s", logs.String())
	}
}

// TestHydrateCompletedChangeReviewSkipsPlainConnector pins the behavior of
// every tracker that does not review changes of its own: nothing is asked and
// nothing changes.
func TestHydrateCompletedChangeReviewSkipsPlainConnector(t *testing.T) {
	t.Parallel()

	issue := completionTransitionIssue("In Progress", "OPEN")
	orch := &Orchestrator{connector: &autoPromoteTickConnector{}}

	if hydrated := orch.hydrateCompletedChangeReview(t.Context(), issue); hydrated.ChangeReview != nil {
		t.Fatalf("change review = %#v, want none from a connector that cannot report one", hydrated.ChangeReview)
	}
}
