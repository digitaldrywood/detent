package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type crossHostParkConnector struct {
	*dependencyAutoUnblockConnector
	now            time.Time
	commentErr     error
	instanceLogin  string
	updateDelay    time.Duration
	afterUpdate    func()
	updateErr      error
	commentFailure func(string) error
}

func (c *crossHostParkConnector) InstanceLogin() string {
	return c.instanceLogin
}

func (c *crossHostParkConnector) UpdateIssueState(ctx context.Context, issueID, state string) error {
	if c.updateErr != nil {
		return c.updateErr
	}
	c.now = c.now.Add(c.updateDelay)
	if err := c.dependencyAutoUnblockConnector.UpdateIssueState(ctx, issueID, state); err != nil {
		return err
	}
	for i := range c.stateIssues {
		if c.stateIssues[i].ID == issueID {
			c.stateIssues[i].State = state
			c.stateIssues[i].StageUpdatedAt = timePointer(c.now)
			c.stateIssues[i].StageUpdatedActor = connector.IssueActor{Login: "shared-user", Kind: "User"}
		}
	}
	if c.afterUpdate != nil {
		c.afterUpdate()
	}
	return nil
}

func (c *crossHostParkConnector) CreateComment(ctx context.Context, issueID, body string) error {
	if c.commentFailure != nil {
		if err := c.commentFailure(body); err != nil {
			return err
		}
	}
	if err := c.dependencyAutoUnblockConnector.CreateComment(ctx, issueID, body); err != nil {
		return err
	}
	for i := range c.stateIssues {
		if c.stateIssues[i].ID == issueID {
			c.stateIssues[i].Comments = append(c.stateIssues[i].Comments, connector.IssueComment{
				Body: body, AuthorLogin: "shared-user", AuthorKind: "User", AuthorAuthorized: true, CreatedAt: timePointer(c.now),
			})
		}
	}
	return nil
}

func (c *crossHostParkConnector) FetchIssueComments(_ context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	if c.commentErr != nil {
		return nil, c.commentErr
	}
	for _, current := range c.stateIssues {
		if current.ID == issue.ID {
			return cloneIssueComments(current.Comments), nil
		}
	}
	return nil, nil
}

func (c *crossHostParkConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	var issues []connector.Issue
	for _, id := range ids {
		for _, issue := range c.stateIssues {
			if issue.ID == id {
				issues = append(issues, cloneIssue(issue))
			}
		}
	}
	return issues, nil
}

func TestDerivedHoldDispatchEquivalence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name                       string
		cause                      string
		human, unresolved, stopped bool
		want                       int
	}{
		{name: "ready", want: 2},
		{name: "instant failure", cause: instantFailureCircuitBreakerCause, want: 2},
		{name: "repeated failure", cause: repeatedFailureCircuitBreakerCause, want: 2},
		{name: "spend history", cause: spendProgressReason, want: 2},
		{name: "dispatch loop", cause: dispatchLoopDetectedReason, want: 2},
		{name: "token ceiling", cause: "token_ceiling_circuit_breaker", want: 2},
		{name: "workspace retries", cause: workspacePreparationRetryLimitCause, want: 2},
		{name: "current dependency", unresolved: true},
		{name: "current human action", human: true},
		{name: "operator stop", stopped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, wipe := range []bool{false, true} {
				now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
				blocker := dependencyAutoUnblockIssue("dependency", "Done")
				if test.unresolved {
					blocker.State = "In Progress"
				}
				var issues []connector.Issue
				for _, id := range []string{"first", "second"} {
					issue := dependencyAutoUnblockIssue(id, "Blocked")
					issue.Identifier = "example-org/example-project#" + id
					issue.BlockedBy = []connector.BlockedRef{{Identifier: blocker.Identifier, Source: connector.BlockedRefSourceNative}}
					if test.human {
						issue.WorkpadSignal = &workpad.Signal{Status: workpad.StatusBlocked, HumanAction: "Approve scope"}
					}
					if !wipe {
						issue.BlockerReason = firstNonBlank(test.cause, noProgressLimitReason)
						issue.Comments = []connector.IssueComment{{AuthorAuthorized: true, Body: "## Detent recovery park\n\n```detent-park\ninvalid\n```"}}
					}
					issues = append(issues, issue)
				}
				tracker := &dependencyAutoUnblockConnector{stateIssues: issues, blockers: []connector.Issue{blocker}}
				host := dependencyAutoUnblockOrchestrator(tracker, DependencyAutoUnblockConfig{Enabled: true})
				metrics := &autoPromoteWorkflowMetricsRecorder{}
				host.workflowMetrics = metrics
				state := newState(host.cfg)
				for _, issue := range issues {
					if !wipe {
						recordDependencyLaneEntry(t, metrics, issue, "Blocked", firstNonBlank(test.cause, noProgressLimitReason), now, workflowLaneMetadata{BlockedRecovery: &workflowLaneBlockedRecoveryMetadata{Owner: "orchestrator", Predicate: "managed", Cause: firstNonBlank(test.cause, noProgressLimitReason)}})
					}
					if test.stopped {
						state.Blocked[issue.ID] = Blocked{Issue: issue, Source: BlockedSourceOperatorStop}
					}
				}
				got := host.autoUnblockDependencyIssues(t.Context(), &state, issues, now)
				if len(got) != test.want {
					t.Fatalf("wipe=%t: recovered %d items, want %d", wipe, len(got), test.want)
				}
			}
		})
	}
}
