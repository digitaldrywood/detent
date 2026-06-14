package memory

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	EventKindComment        EventKind = "memory_tracker_comment"
	EventKindStateUpdate    EventKind = "memory_tracker_state_update"
	EventKindAssigneeUpdate EventKind = "memory_tracker_assignee_update"
	EventKindFieldUpdate    EventKind = "memory_tracker_field_update"
	EventKindClose          EventKind = "memory_tracker_close"
)

type EventKind string

type Event struct {
	Kind    EventKind
	IssueID string
	Body    string
	State   string
	Login   string

	FieldName  string
	FieldValue string
}

type EventSink func(Event)

type Config struct {
	Issues    []connector.Issue
	EventSink EventSink
	Stateful  bool
	Now       func() time.Time
}

type Connector struct {
	issues    []connector.Issue
	eventSink EventSink
	stateful  bool
	now       func() time.Time
	mu        sync.RWMutex
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.InstanceIdentifier = (*Connector)(nil)
var _ connector.IssueChildrenResolver = (*Connector)(nil)
var _ connector.IssueCloser = (*Connector)(nil)
var _ connector.IssueParentResolver = (*Connector)(nil)
var _ connector.IssueReferenceResolver = (*Connector)(nil)

func New(cfg Config) *Connector {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Connector{
		issues:    cloneIssues(cfg.Issues),
		eventSink: cfg.EventSink,
		stateful:  cfg.Stateful,
		now:       now,
	}
}

func (c *Connector) Name() string {
	return connector.BackendMemory.String()
}

func (c *Connector) InstanceLogin() string {
	return ""
}

func (c *Connector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return cloneIssues(c.issues), nil
}

func (c *Connector) FetchIssuesByStates(_ context.Context, stateNames []string) ([]connector.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

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
	c.mu.RLock()
	defer c.mu.RUnlock()

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

func (c *Connector) FetchIssueStatesByIdentifiers(_ context.Context, identifiers []string) ([]connector.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	wantedIdentifiers := make(map[string]struct{}, len(identifiers))
	for _, identifier := range identifiers {
		wantedIdentifiers[normalizeState(identifier)] = struct{}{}
	}

	issues := make([]connector.Issue, 0, len(c.issues))
	for _, issue := range c.issues {
		if _, ok := wantedIdentifiers[normalizeState(issue.Identifier)]; ok {
			issues = append(issues, cloneIssue(issue))
		}
	}

	return issues, nil
}

func (c *Connector) FetchIssueParents(_ context.Context, issueID string) ([]connector.Issue, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return []connector.Issue{}, nil
	}

	childIdentifier := ""
	for _, issue := range c.issues {
		if strings.TrimSpace(issue.ID) == issueID {
			childIdentifier = normalizeState(issue.Identifier)
			break
		}
	}

	parents := []connector.Issue{}
	seen := map[string]struct{}{}
	for _, issue := range c.issues {
		if strings.TrimSpace(issue.ID) == issueID {
			continue
		}
		if !issueReferencesChild(issue, issueID, childIdentifier) {
			continue
		}
		key := memoryIssueKey(issue)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		parents = append(parents, cloneIssue(issue))
	}

	return parents, nil
}

func (c *Connector) FetchIssueChildren(_ context.Context, issueID string) ([]connector.BlockedRef, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, issue := range c.issues {
		if issue.ID == issueID {
			return append([]connector.BlockedRef(nil), issue.ChildIssues...), nil
		}
	}
	return []connector.BlockedRef{}, nil
}

func (c *Connector) CreateComment(_ context.Context, issueID string, body string) error {
	c.send(Event{Kind: EventKindComment, IssueID: issueID, Body: body})
	return nil
}

func (c *Connector) CloseIssue(_ context.Context, issueID string) error {
	c.applyIssue(issueID, func(issue *connector.Issue, now time.Time) {
		issue.Closed = true
		issue.UpdatedAt = &now
	})
	c.send(Event{Kind: EventKindClose, IssueID: issueID})
	return nil
}

func (c *Connector) UpdateIssueState(_ context.Context, issueID string, stateName string) error {
	c.applyIssue(issueID, func(issue *connector.Issue, now time.Time) {
		issue.State = stateName
		issue.StageUpdatedAt = &now
		issue.UpdatedAt = &now
		if issue.Fields == nil {
			issue.Fields = map[string]string{}
		}
		issue.Fields["Status"] = stateName
	})
	c.send(Event{Kind: EventKindStateUpdate, IssueID: issueID, State: stateName})
	return nil
}

func (c *Connector) SetAssignee(_ context.Context, issueID string, login string) error {
	c.applyIssue(issueID, func(issue *connector.Issue, now time.Time) {
		issue.AssigneeID = login
		if !stringSliceContains(issue.Assignees, login) {
			issue.Assignees = append(issue.Assignees, login)
		}
		issue.UpdatedAt = &now
	})
	c.send(Event{Kind: EventKindAssigneeUpdate, IssueID: issueID, Login: login})
	return nil
}

func (c *Connector) SetField(_ context.Context, issueID string, fieldName string, value string) error {
	c.applyIssue(issueID, func(issue *connector.Issue, now time.Time) {
		if issue.Fields == nil {
			issue.Fields = map[string]string{}
		}
		issue.Fields[fieldName] = value
		issue.UpdatedAt = &now
	})
	c.send(Event{Kind: EventKindFieldUpdate, IssueID: issueID, FieldName: fieldName, FieldValue: value})
	return nil
}

func (c *Connector) applyIssue(issueID string, update func(*connector.Issue, time.Time)) {
	if !c.stateful {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	for index := range c.issues {
		if c.issues[index].ID != issueID {
			continue
		}
		update(&c.issues[index], now)
		return
	}
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

func issueReferencesChild(issue connector.Issue, childID string, childIdentifier string) bool {
	for _, ref := range issue.ChildIssues {
		if refReferencesIssue(ref, childID, childIdentifier) {
			return true
		}
	}
	for _, ref := range issue.BlockedBy {
		if refReferencesIssue(ref, childID, childIdentifier) {
			return true
		}
	}
	return false
}

func refReferencesIssue(ref connector.BlockedRef, issueID string, issueIdentifier string) bool {
	if strings.TrimSpace(ref.ID) == issueID {
		return true
	}
	if issueIdentifier != "" && normalizeState(ref.Identifier) == issueIdentifier {
		return true
	}
	return false
}

func memoryIssueKey(issue connector.Issue) string {
	if id := strings.TrimSpace(issue.ID); id != "" {
		return "id:" + id
	}
	if identifier := normalizeState(issue.Identifier); identifier != "" {
		return "identifier:" + identifier
	}
	return ""
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
		pullRequest.ActivityAt = cloneTime(issue.PullRequest.ActivityAt)
		if issue.PullRequest.CodexReviewSubmittedAt != nil {
			submittedAt := *issue.PullRequest.CodexReviewSubmittedAt
			pullRequest.CodexReviewSubmittedAt = &submittedAt
		}
		if issue.PullRequest.LatestCodexReviewSubmittedAt != nil {
			submittedAt := *issue.PullRequest.LatestCodexReviewSubmittedAt
			pullRequest.LatestCodexReviewSubmittedAt = &submittedAt
		}
		pullRequest.CodexReviewFindings = append([]connector.PullRequestFinding(nil), issue.PullRequest.CodexReviewFindings...)
		issue.PullRequest = &pullRequest
	}
	if issue.BlockedBy != nil {
		issue.BlockedBy = append([]connector.BlockedRef(nil), issue.BlockedBy...)
	}
	if issue.ChildIssues != nil {
		issue.ChildIssues = append([]connector.BlockedRef(nil), issue.ChildIssues...)
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
	maps.Copy(out, values)
	return out
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}

	cloned := *value
	return &cloned
}

func stringSliceContains(values []string, want string) bool {
	return slices.Contains(values, want)
}
