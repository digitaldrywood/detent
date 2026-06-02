package memory

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	EventKindComment     EventKind = "memory_tracker_comment"
	EventKindStateUpdate EventKind = "memory_tracker_state_update"
)

type EventKind string

type Event struct {
	Kind    EventKind
	IssueID string
	Body    string
	State   string
}

type EventSink func(Event)

type Config struct {
	Issues    []connector.Issue
	EventSink EventSink
}

type Connector struct {
	issues    []connector.Issue
	eventSink EventSink
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.InstanceIdentifier = (*Connector)(nil)

func New(cfg Config) *Connector {
	return &Connector{
		issues:    cloneIssues(cfg.Issues),
		eventSink: cfg.EventSink,
	}
}

func (c *Connector) Name() string {
	return connector.BackendMemory.String()
}

func (c *Connector) InstanceLogin() string {
	return ""
}

func (c *Connector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return cloneIssues(c.issues), nil
}

func (c *Connector) FetchIssuesByStates(_ context.Context, stateNames []string) ([]connector.Issue, error) {
	wantedStates := make(map[string]struct{}, len(stateNames))
	for _, stateName := range stateNames {
		wantedStates[normalizeState(stateName)] = struct{}{}
	}

	issues := make([]connector.Issue, 0, len(c.issues))
	for _, issue := range c.issues {
		if _, ok := wantedStates[normalizeState(issue.State)]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}

	return issues, nil
}

func (c *Connector) FetchIssueStatesByIDs(_ context.Context, issueIDs []string) ([]connector.Issue, error) {
	wantedIDs := make(map[string]struct{}, len(issueIDs))
	for _, issueID := range issueIDs {
		wantedIDs[issueID] = struct{}{}
	}

	issues := make([]connector.Issue, 0, len(c.issues))
	for _, issue := range c.issues {
		if _, ok := wantedIDs[issue.ID]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}

	return issues, nil
}

func (c *Connector) CreateComment(_ context.Context, issueID string, body string) error {
	c.send(Event{Kind: EventKindComment, IssueID: issueID, Body: body})
	return nil
}

func (c *Connector) UpdateIssueState(_ context.Context, issueID string, stateName string) error {
	c.send(Event{Kind: EventKindStateUpdate, IssueID: issueID, State: stateName})
	return nil
}

func (c *Connector) send(event Event) {
	if c.eventSink == nil {
		return
	}

	c.eventSink(event)
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func cloneIssues(issues []connector.Issue) []connector.Issue {
	out := make([]connector.Issue, len(issues))
	for i, issue := range issues {
		out[i] = cloneIssue(issue)
	}
	return out
}

func cloneIssue(issue connector.Issue) connector.Issue {
	if issue.Priority != nil {
		priority := *issue.Priority
		issue.Priority = &priority
	}
	if issue.PRNumber != nil {
		prNumber := *issue.PRNumber
		issue.PRNumber = &prNumber
	}
	if issue.PullRequest != nil {
		pullRequest := *issue.PullRequest
		issue.PullRequest = &pullRequest
	}
	if issue.BlockedBy != nil {
		issue.BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
	}
	if issue.Labels != nil {
		issue.Labels = append([]string(nil), issue.Labels...)
	}
	if issue.Assignees != nil {
		issue.Assignees = cloneStringSlice(issue.Assignees)
	}
	if issue.Fields != nil {
		issue.Fields = cloneStringMap(issue.Fields)
	}
	issue.CreatedAt = cloneTime(issue.CreatedAt)
	issue.UpdatedAt = cloneTime(issue.UpdatedAt)
	issue.StageUpdatedAt = cloneTime(issue.StageUpdatedAt)

	return issue
}

func cloneStringSlice(values []string) []string {
	return append([]string{}, values...)
}

func cloneStringMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	cloned := *value
	return &cloned
}
