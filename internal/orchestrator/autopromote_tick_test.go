package orchestrator

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
)

func TestTickAutoPromoteHumanReviewIssues(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	recentReview := now.Add(-30 * time.Second)

	tests := []struct {
		name                 string
		cfg                  AutoPromoteConfig
		issue                connector.Issue
		wantUpdates          []autoPromoteTickUpdate
		wantCommentFragments []string
		wantLogFragments     []string
		rejectLogFragments   []string
	}{
		{
			name: "promotes ready issue to merging",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-ready", []string{"bug"}, &connector.PullRequest{
				Number:                 42,
				URL:                    "https://github.test/digitaldrywood/detent/pull/42",
				State:                  "OPEN",
				CIStatus:               "success",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-ready",
				state:   "Merging",
			}},
			wantCommentFragments: []string{
				"Auto-promoted this issue from Human Review to Merging.",
				"reason: ready",
				"https://github.test/digitaldrywood/detent/pull/42",
			},
		},
		{
			name: "linked pull request without required automated review waits for review",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-linked-missing-review", []string{"bug"}, &connector.PullRequest{
				Number:     390,
				URL:        "https://github.test/digitaldrywood/detent/pull/390",
				BranchName: "detent/detent-digitaldrywood_detent_387-29d3e4765f21",
				State:      "OPEN",
				CIStatus:   "pass",
			}),
			wantLogFragments: []string{
				"reason=automated_review_missing",
			},
			rejectLogFragments: []string{
				"reason=missing_pull_request",
			},
		},
		{
			name: "routes P1 findings to rework",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-p1", []string{"bug"}, &connector.PullRequest{
				Number:                 43,
				URL:                    "https://github.test/digitaldrywood/detent/pull/43",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "P1",
				CodexReviewSubmittedAt: &oldReview,
				CodexReviewFindings: []connector.PullRequestFinding{{
					Body: "![P1 Badge](https://example.test/p1.svg) Unsafe migration.",
					URL:  "https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
				}},
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-p1",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework.",
				"reason: p1_findings",
				"Unsafe migration.",
				"https://github.test/digitaldrywood/detent/pull/43#pullrequestreview-1",
			},
		},
		{
			name: "routes failing ci to rework when configured",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
				Gate: gate.Config{
					Kind:            gate.KindCommand,
					CIFailureAction: gate.CIFailureActionRework,
				},
			},
			issue: autoPromoteTickIssue("issue-red-ci", []string{"bug"}, &connector.PullRequest{
				Number:   48,
				URL:      "https://github.test/digitaldrywood/detent/pull/48",
				State:    "OPEN",
				CIStatus: "fail",
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-red-ci",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework: current-head CI is failing.",
				"reason: ci_not_green",
				"ci_status: red",
				"https://github.test/digitaldrywood/detent/pull/48",
			},
		},
		{
			name: "routes conflicting pull request to rework",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 0,
				Gate: gate.Config{
					Kind:            gate.KindCommand,
					CIFailureAction: gate.CIFailureActionRework,
				},
			},
			issue: autoPromoteTickIssue("issue-conflicting-pr", []string{"bug"}, &connector.PullRequest{
				Number:         49,
				URL:            "https://github.test/digitaldrywood/detent/pull/49",
				State:          "OPEN",
				MergeableState: "dirty",
			}),
			wantUpdates: []autoPromoteTickUpdate{{
				issueID: "issue-conflicting-pr",
				state:   "Rework",
			}},
			wantCommentFragments: []string{
				"Auto-promote routed this issue from Human Review to Rework: linked PR has merge conflicts.",
				"reason: merge_conflicts",
				"https://github.test/digitaldrywood/detent/pull/49",
			},
			wantLogFragments: []string{
				"reason=merge_conflicts",
				"target_state=Rework",
			},
		},
		{
			name: "waits for quiet period",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-recent", []string{"bug"}, &connector.PullRequest{
				Number:                 44,
				URL:                    "https://github.test/digitaldrywood/detent/pull/44",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &recentReview,
			}),
		},
		{
			name: "skips closed pull request",
			cfg: AutoPromoteConfig{
				Enabled:       true,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-closed-pr", []string{"bug"}, &connector.PullRequest{
				Number:                 47,
				URL:                    "https://github.test/digitaldrywood/detent/pull/47",
				State:                  "MERGED",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
		{
			name: "honors evaluator label filters",
			cfg: AutoPromoteConfig{
				Enabled:            true,
				QuietDuration:      10 * time.Minute,
				AllowedIssueLabels: []string{"release"},
			},
			issue: autoPromoteTickIssue("issue-label", []string{"bug"}, &connector.PullRequest{
				Number:                 45,
				URL:                    "https://github.test/digitaldrywood/detent/pull/45",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
		{
			name: "disabled config does not evaluate",
			cfg: AutoPromoteConfig{
				Enabled:       false,
				QuietDuration: 10 * time.Minute,
			},
			issue: autoPromoteTickIssue("issue-disabled", []string{"bug"}, &connector.PullRequest{
				Number:                 46,
				URL:                    "https://github.test/digitaldrywood/detent/pull/46",
				State:                  "OPEN",
				CIStatus:               "pass",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &oldReview,
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				AutoPromote:         tt.cfg,
				ActiveStates:        []string{"Todo", "In Progress", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{tt.issue}}
			var logs strings.Builder
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			}

			orch.tick(context.Background(), &state, now)

			if !reflect.DeepEqual(tracker.updates, tt.wantUpdates) {
				t.Fatalf("updates = %#v, want %#v", tracker.updates, tt.wantUpdates)
			}
			if len(tracker.fetchByStatesRequests) != 1 {
				t.Fatalf("FetchIssuesByStates() calls = %d, want 1", len(tracker.fetchByStatesRequests))
			}
			if !autoPromoteTickStatesEqual(tracker.fetchByStatesRequests[0], []string{"Blocked", "Human Review", "Merging"}) {
				t.Fatalf("FetchIssuesByStates() states = %#v, want Blocked/Human Review/Merging", tracker.fetchByStatesRequests[0])
			}
			if len(tt.wantCommentFragments) == 0 {
				if len(tracker.comments) != 0 {
					t.Fatalf("comments = %#v, want none", tracker.comments)
				}
			} else {
				if len(tracker.comments) != 1 {
					t.Fatalf("comments = %#v, want one comment", tracker.comments)
				}
				for _, fragment := range tt.wantCommentFragments {
					if !strings.Contains(tracker.comments[0].body, fragment) {
						t.Fatalf("comment %q missing fragment %q", tracker.comments[0].body, fragment)
					}
				}
			}
			for _, fragment := range tt.wantLogFragments {
				if !strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
				}
			}
			for _, fragment := range tt.rejectLogFragments {
				if strings.Contains(logs.String(), fragment) {
					t.Fatalf("logs %q contain rejected fragment %q", logs.String(), fragment)
				}
			}
		})
	}
}

func TestTickAutoPromoteLogsNonTransitionDecisionsAtInfo(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 22, 14, 0, 0, 0, time.UTC)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-missing-review", []string{"bug"}, &connector.PullRequest{
		Number:     390,
		URL:        "https://github.test/digitaldrywood/detent/pull/390",
		BranchName: "detent/detent-digitaldrywood_detent_387-29d3e4765f21",
		State:      "OPEN",
		CIStatus:   "pass",
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	if len(tracker.updates) != 0 {
		t.Fatalf("updates = %#v, want none", tracker.updates)
	}
	for _, fragment := range []string{
		"level=INFO",
		"auto promote decision",
		"action=await_review",
		"reason=automated_review_missing",
	} {
		if !strings.Contains(logs.String(), fragment) {
			t.Fatalf("logs %q missing fragment %q", logs.String(), fragment)
		}
	}
}

func TestTickAutoPromoteRunsValidatorStage(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	oldReview := now.Add(-20 * time.Minute)
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		AutoPromote: AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 10 * time.Minute,
			Gate: gate.Config{
				Kind: gate.KindCommand,
				Validator: gate.ValidatorConfig{
					Enabled:  true,
					MinScore: 0.8,
					BlockOn:  []string{"p1"},
				},
			},
		},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		TerminalStates: []string{"Done", "Cancelled"},
	})
	issue := autoPromoteTickIssue("issue-validator", []string{"enhancement"}, &connector.PullRequest{
		Number:                 522,
		URL:                    "https://github.test/digitaldrywood/detent/pull/522",
		BranchName:             "detent/digitaldrywood_detent_522",
		HeadSHA:                "head-validator",
		State:                  "OPEN",
		CIStatus:               "success",
		CodexReviewState:       "COMMENTED",
		CodexReviewSubmittedAt: &oldReview,
	})
	tracker := &autoPromoteTickConnector{stateIssues: []connector.Issue{issue}}
	validator := &autoPromoteTickValidator{
		result: gate.ValidatorResult{
			Submitted: true,
			Verdict:   gate.ValidatorVerdictPass,
			Score:     0.91,
			Summary:   "Acceptance criteria pass.",
		},
	}
	var logs strings.Builder
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		validator: validator,
		logger:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	state := newState(cfg)
	orch.tick(context.Background(), &state, now)

	waitForValidatorRequests(t, validator, 1)
	waitForValidatorResult(t, orch, issue)
	if got := tracker.updates; len(got) != 0 {
		t.Fatalf("updates after scheduling validator = %#v, want none", got)
	}
	requests := validator.Requests()
	if requests[0].Issue.ID != "issue-validator" {
		t.Fatalf("validator issue = %#v, want issue-validator", requests[0].Issue)
	}

	orch.tick(context.Background(), &state, now.Add(time.Second))

	if got, want := tracker.updates, []autoPromoteTickUpdate{{issueID: "issue-validator", state: "Merging"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	if len(tracker.prComments) != 1 {
		t.Fatalf("pull request comments = %#v, want validator result comment", tracker.prComments)
	}
	for _, fragment := range []string{"Validator verdict: pass", "score: 0.91", "Acceptance criteria pass."} {
		if !strings.Contains(tracker.prComments[0].body, fragment) {
			t.Fatalf("pull request comment %q missing %q", tracker.prComments[0].body, fragment)
		}
	}
}

func autoPromoteTickIssue(id string, labels []string, pullRequest *connector.PullRequest) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = id
	issue.Identifier = "digitaldrywood/detent#42"
	issue.Title = "Auto promote test"
	issue.State = "Human Review"
	issue.Labels = append([]string(nil), labels...)
	issue.PullRequest = pullRequest
	return issue
}

func autoPromoteTickStatesEqual(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]struct{}, len(got))
	for _, state := range got {
		seen[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	for _, state := range want {
		if _, ok := seen[strings.ToLower(strings.TrimSpace(state))]; !ok {
			return false
		}
	}
	return true
}

type autoPromoteTickUpdate struct {
	issueID string
	state   string
}

type autoPromoteTickComment struct {
	issueID string
	body    string
}

type autoPromoteTickConnector struct {
	stateIssues           []connector.Issue
	fetchByStatesRequests [][]string
	updates               []autoPromoteTickUpdate
	comments              []autoPromoteTickComment
	prComments            []autoPromoteTickComment
}

func (c *autoPromoteTickConnector) Name() string {
	return "auto-promote-tick"
}

func (c *autoPromoteTickConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *autoPromoteTickConnector) FetchIssuesByStates(_ context.Context, states []string) ([]connector.Issue, error) {
	c.fetchByStatesRequests = append(c.fetchByStatesRequests, append([]string(nil), states...))
	wanted := make(map[string]struct{}, len(states))
	for _, state := range states {
		wanted[strings.ToLower(strings.TrimSpace(state))] = struct{}{}
	}
	issues := make([]connector.Issue, 0, len(c.stateIssues))
	for _, issue := range c.stateIssues {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(issue.State))]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}
	return issues, nil
}

func (c *autoPromoteTickConnector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *autoPromoteTickConnector) CreateComment(_ context.Context, issueID string, body string) error {
	c.comments = append(c.comments, autoPromoteTickComment{issueID: issueID, body: body})
	return nil
}

func (c *autoPromoteTickConnector) CreatePullRequestComment(_ context.Context, repository string, number int, body string) error {
	c.prComments = append(c.prComments, autoPromoteTickComment{issueID: repository, body: body})
	return nil
}

type autoPromoteTickValidator struct {
	mu       sync.Mutex
	result   gate.ValidatorResult
	requests []ValidatorRequest
	err      error
}

func (v *autoPromoteTickValidator) Validate(_ context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.requests = append(v.requests, req)
	return v.result, v.err
}

func (v *autoPromoteTickValidator) Requests() []ValidatorRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]ValidatorRequest(nil), v.requests...)
}

func waitForValidatorRequests(t *testing.T, validator *autoPromoteTickValidator, count int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(validator.Requests()) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("validator requests = %d, want at least %d", len(validator.Requests()), count)
}

func waitForValidatorResult(t *testing.T, orch *Orchestrator, issue connector.Issue) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := orch.validatorStageResult(issue); ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("validator result was not recorded")
}

func (c *autoPromoteTickConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, autoPromoteTickUpdate{issueID: issueID, state: state})
	return nil
}

func (c *autoPromoteTickConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *autoPromoteTickConnector) SetField(context.Context, string, string, string) error {
	return nil
}
