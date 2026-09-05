package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeConnector struct {
	client *NativeClient
}

func NewNativeConnector(client *NativeClient) (*NativeConnector, error) {
	if client == nil {
		return nil, errors.New("native Hub client is required")
	}
	return &NativeConnector{client: client}, nil
}

func (c *NativeConnector) Name() string { return "hub_native" }

func (c *NativeConnector) Capabilities() connector.Capabilities {
	return connector.Capabilities{UpdateIssueState: true, SetAssignee: true, SetField: true, CreateComment: true, CreateWorkItems: true, UpdateComments: true}
}

func (c *NativeConnector) FetchCandidateIssues(ctx context.Context) ([]connector.Issue, error) {
	project, err := c.client.Project(ctx)
	if err != nil {
		return nil, err
	}
	var states []string
	for _, state := range project.States {
		if state.Dispatchable && !state.Terminal {
			states = append(states, state.Name)
		}
	}
	return c.FetchIssuesByStates(ctx, states)
}

func (c *NativeConnector) FetchIssuesByStates(ctx context.Context, states []string) ([]connector.Issue, error) {
	var issues []connector.Issue
	for _, state := range states {
		cursor := ""
		for {
			page, err := c.client.Issues(ctx, url.Values{"state": {state}, "limit": {"2"}, "cursor": {cursor}})
			if err != nil {
				return nil, err
			}
			for _, issue := range page.Items {
				issues = append(issues, issueFromNative(issue))
			}
			connector.ReportProgress(ctx)
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				return nil, errors.New("hub repeated issue cursor")
			}
			cursor = page.NextCursor
		}
	}
	return issues, nil
}

func (c *NativeConnector) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]connector.Issue, error) {
	issues := make([]connector.Issue, 0, len(ids))
	for _, id := range ids {
		issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				continue
			}
			return nil, err
		}
		issues = append(issues, issueFromNative(issue))
	}
	return issues, nil
}

func nativeMutationKey() tracker.Mutation { return tracker.Mutation{IdempotencyKey: uuid.NewString()} }

func (c *NativeConnector) CreateIssue(ctx context.Context, draft connector.IssueDraft) (connector.Issue, error) {
	project, err := c.client.Project(ctx)
	if err != nil {
		return connector.Issue{}, err
	}
	if len(project.States) == 0 {
		return connector.Issue{}, errors.New("native project has no workflow states")
	}
	issue, err := c.client.CreateIssue(ctx, tracker.CreateIssue{Mutation: nativeMutationKey(), Title: draft.Title, Body: draft.Body, Labels: draft.Labels, State: project.States[0].Name})
	return issueFromNative(issue), err
}

func (c *NativeConnector) CreateComment(ctx context.Context, id, body string) error {
	_, err := c.client.CreateComment(ctx, tracker.NativeWorkItemID(id), tracker.CreateComment{Mutation: nativeMutationKey(), Body: body})
	return err
}

func (c *NativeConnector) UpdateIssueState(ctx context.Context, id, state string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	if issue.State == state {
		return nil
	}
	_, err = c.client.Transition(ctx, issue.WorkItemID, tracker.Transition{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, State: state, Reason: "worker_progress"})
	return err
}

func (c *NativeConnector) SetAssignee(ctx context.Context, id, login string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	assignees := []string{}
	if login != "" {
		assignees = append(assignees, login)
	}
	_, err = c.client.UpdateIssue(ctx, issue.WorkItemID, tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, Assignees: &assignees})
	return err
}

func (c *NativeConnector) SetField(ctx context.Context, id, field, value string) error {
	if field == "state" {
		return c.UpdateIssueState(ctx, id, value)
	}
	if field == "assignee" {
		return c.SetAssignee(ctx, id, value)
	}
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	request := tracker.UpdateIssue{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision}
	switch field {
	case "title":
		request.Title = &value
	case "body":
		request.Body = &value
	case "priority":
		priority, err := strconv.Atoi(value)
		if err != nil {
			return err
		}
		request.Priority = &priority
	default:
		return connector.ErrNotImplemented
	}
	_, err = c.client.UpdateIssue(ctx, issue.WorkItemID, request)
	return err
}

func (c *NativeConnector) UpdateIssueBody(ctx context.Context, id, body string) error {
	return c.SetField(ctx, id, "body", body)
}

func (c *NativeConnector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	var comments []connector.IssueComment
	cursor := ""
	for {
		page, err := c.client.Comments(ctx, tracker.NativeWorkItemID(issue.ID), cursor)
		if err != nil {
			return nil, err
		}
		for _, comment := range page.Items {
			mapped := connector.IssueComment{ID: comment.ID, Backend: c.Name(), Body: comment.Body, AuthorLogin: comment.Actor.PrincipalID, AuthorKind: comment.Actor.Kind,
				CreatedAt: cloneTime(&comment.CreatedAt), UpdatedAt: cloneTime(&comment.UpdatedAt), CanEdit: true, TargetType: "issue", AuthorAuthorized: comment.Actor.Kind == "human" && comment.Provenance == nil}
			if comment.Provenance != nil {
				mapped.AuthorLogin = comment.Provenance.AuthorID
				mapped.AuthorDisplayName = comment.Provenance.AuthorDisplayName
				mapped.AuthorKind = "imported"
			}
			comments = append(comments, mapped)
		}
		connector.ReportProgress(ctx)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, errors.New("hub repeated comment cursor")
		}
		cursor = page.NextCursor
	}
	return comments, nil
}

func (c *NativeConnector) IsIssueCommentAuthorAuthorized(_ context.Context, _ connector.Issue, comment connector.IssueComment) (bool, error) {
	return comment.Backend == c.Name() && comment.AuthorKind == "human" && comment.AuthorAuthorized, nil
}

func (c *NativeConnector) UpdateIssueComment(ctx context.Context, id, commentID, body string) error {
	cursor := ""
	for {
		page, err := c.client.Comments(ctx, tracker.NativeWorkItemID(id), cursor)
		if err != nil {
			return err
		}
		for _, comment := range page.Items {
			if comment.ID == commentID {
				_, err := c.client.UpdateComment(ctx, tracker.NativeWorkItemID(id), commentID, tracker.UpdateComment{Mutation: nativeMutationKey(), ExpectedRevision: comment.Revision, Body: body})
				return err
			}
		}
		if page.NextCursor == "" {
			return errors.New("native comment was not found")
		}
		if page.NextCursor == cursor {
			return errors.New("hub repeated comment cursor")
		}
		cursor = page.NextCursor
	}
}

func (c *NativeConnector) FetchIssueEvents(ctx context.Context, issue connector.Issue) ([]connector.IssueEvent, error) {
	var events []connector.IssueEvent
	cursor := ""
	for {
		page, err := c.client.History(ctx, tracker.NativeWorkItemID(issue.ID), cursor)
		if err != nil {
			return nil, err
		}
		for _, event := range page.Items {
			events = append(events, connector.IssueEvent{ID: event.ID, Kind: event.Type, State: event.Data.ToState, CreatedAt: cloneTime(&event.RecordedAt), Actor: connector.IssueActor{Login: event.Actor.PrincipalID}})
		}
		connector.ReportProgress(ctx)
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return nil, errors.New("hub repeated history cursor")
		}
		cursor = page.NextCursor
	}
	return events, nil
}

func (c *NativeConnector) AddIssueDependency(ctx context.Context, issueID, blockerID string) error {
	return c.dependency(ctx, issueID, blockerID, "add")
}

func (c *NativeConnector) RemoveIssueDependency(ctx context.Context, issueID, blockerID string) error {
	return c.dependency(ctx, issueID, blockerID, "remove")
}

func (c *NativeConnector) dependency(ctx context.Context, id, blockerID, operation string) error {
	issue, err := c.client.Issue(ctx, tracker.NativeWorkItemID(id))
	if err != nil {
		return err
	}
	_, err = c.client.Dependency(ctx, issue.WorkItemID, tracker.DependencyMutation{Mutation: nativeMutationKey(), ExpectedRevision: issue.Revision, RelatedWorkItemID: tracker.NativeWorkItemID(blockerID), Operation: operation})
	return err
}

func issueFromNative(native tracker.NativeIssue) connector.Issue {
	issue := connector.NewIssue()
	issue.ID = string(native.WorkItemID)
	issue.Identifier = string(native.ProjectID) + "#" + strconv.Itoa(native.Number)
	issue.Number = native.Number
	issue.Title, issue.Description, issue.State = native.Title, native.Body, native.State
	issue.AuthorID = native.Actor.PrincipalID
	issue.Closed = native.Terminal
	issue.Labels, issue.Assignees = native.Labels, native.Assignees
	issue.CreatedAt, issue.UpdatedAt = cloneTime(&native.CreatedAt), cloneTime(&native.UpdatedAt)
	issue.Metadata["hub_organization_id"] = string(native.OrganizationID)
	issue.Metadata["hub_project_id"] = string(native.ProjectID)
	issue.Metadata["hub_revision"] = strconv.FormatInt(int64(native.Revision), 10)
	issue.Metadata["hub_profile"] = "native"
	if native.Priority != nil {
		priority := *native.Priority + 1
		issue.Priority = &priority
		issue.PriorityName = queuePriorityName(*native.Priority)
	}
	for _, blocker := range native.Blockers {
		if native.IgnoreDependencies {
			break
		}
		state := "open"
		if blocker.Terminal {
			state = "closed"
		}
		issue.BlockedBy = append(issue.BlockedBy, connector.BlockedRef{ID: string(blocker.ID), Identifier: string(blocker.ID), State: blocker.State, TrackerState: state, Source: connector.BlockedRefSourceNative})
	}
	if native.Provenance != nil {
		issue.AuthorID = strings.TrimSpace(native.Provenance.AuthorID)
	}
	return issue
}
