package linear

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
)

const issueCommentsPageSize = 100

const issueCommentsQuery = `
query DetentLinearIssueComments($issueId: String!, $first: Int!, $after: String) {
  issue(id: $issueId) {
    comments(first: $first, after: $after, orderBy: createdAt) {
      nodes {
        id
        body
        url
        createdAt
        updatedAt
        user {
          id
          name
          displayName
        }
        externalUser {
          id
          name
          displayName
        }
        botActor {
          id
          name
          userDisplayName
        }
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`

const commentCreateMutation = `
mutation DetentLinearCreateComment($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) {
    success
    comment {
      id
    }
  }
}`

const issueUpdateStateMutation = `
mutation DetentLinearIssueUpdateState($issueId: String!, $stateId: String!) {
  issueUpdate(id: $issueId, input: { stateId: $stateId }) {
    success
  }
}`

const issueUpdateAssigneeMutation = `
mutation DetentLinearIssueUpdateAssignee($issueId: String!, $assigneeId: String) {
  issueUpdate(id: $issueId, input: { assigneeId: $assigneeId }) {
    success
  }
}`

type Config struct {
	Endpoint   string
	APIKey     string
	StateMap   map[string]string
	HTTPClient HTTPClient
	Logger     *slog.Logger
}

type Connector struct {
	client        *Client
	stateMap      map[string]string
	mu            sync.Mutex
	stateIDByTeam map[string]map[string]string
	teamIDByIssue map[string]string
	userIDByLogin map[string]string
}

var _ connector.Connector = (*Connector)(nil)
var _ connector.CapabilityReporter = (*Connector)(nil)
var _ connector.IssueCommentReader = (*Connector)(nil)

func NewConnector(cfg Config) (*Connector, error) {
	client, err := NewClient(ClientConfig(cfg))
	if err != nil {
		return nil, err
	}
	return &Connector{
		client:        client,
		stateMap:      cloneStateMap(cfg.StateMap),
		stateIDByTeam: make(map[string]map[string]string),
		teamIDByIssue: make(map[string]string),
		userIDByLogin: make(map[string]string),
	}, nil
}

func (c *Connector) Name() string {
	return connector.BackendLinear.String()
}

func (*Connector) Capabilities() connector.Capabilities {
	return connector.Capabilities{UpdateIssueState: true, SetAssignee: true, CreateComment: true}
}

func (c *Connector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *Connector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *Connector) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, connector.ErrNotImplemented
}

func (c *Connector) FetchIssueComments(ctx context.Context, issue connector.Issue) ([]connector.IssueComment, error) {
	issueID := linearIssueID(issue)
	if issueID == "" {
		return []connector.IssueComment{}, nil
	}

	comments := []connector.IssueComment{}
	var after *string
	for {
		page, err := c.fetchIssueCommentsPage(ctx, issueID, after)
		if err != nil {
			return nil, err
		}
		for _, comment := range page.Nodes {
			comments = append(comments, connectorIssueComment(comment))
		}
		if !page.PageInfo.HasNextPage {
			return comments, nil
		}
		cursor := strings.TrimSpace(page.PageInfo.EndCursor)
		if cursor == "" {
			return nil, ErrInvalidResponse
		}
		after = &cursor
	}
}

func (c *Connector) CreateComment(ctx context.Context, issueID string, body string) error {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ErrMissingIssue
	}

	var response struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	if err := c.client.GraphQL(ctx, commentCreateMutation, map[string]any{
		"issueId": issueID,
		"body":    body,
	}, &response); err != nil {
		return fmt.Errorf("create linear comment: %w", err)
	}
	if !response.CommentCreate.Success || response.CommentCreate.Comment == nil || strings.TrimSpace(response.CommentCreate.Comment.ID) == "" {
		return ErrCommentCreateFailed
	}

	return nil
}

func (c *Connector) UpdateIssueState(ctx context.Context, issueID string, state string) error {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ErrMissingIssue
	}

	_, usedCachedState := c.cachedStateIDForState(issueID, state)
	stateID, err := c.resolveStateID(ctx, issueID, state)
	if err != nil {
		if errors.Is(err, ErrIssueNotFound) || errors.Is(err, ErrStateNotFound) {
			return err
		}
		return fmt.Errorf("update linear issue state: %w", err)
	}

	success, err := c.updateIssueStateID(ctx, issueID, stateID)
	if err != nil {
		return fmt.Errorf("update linear issue state: %w", err)
	}
	if success {
		return nil
	}

	if usedCachedState {
		c.invalidateIssueStateCache(issueID)
		stateID, err = c.resolveStateID(ctx, issueID, state)
		if err != nil {
			if errors.Is(err, ErrIssueNotFound) || errors.Is(err, ErrStateNotFound) {
				return err
			}
			return fmt.Errorf("update linear issue state: %w", err)
		}
		success, err = c.updateIssueStateID(ctx, issueID, stateID)
		if err != nil {
			return fmt.Errorf("update linear issue state: %w", err)
		}
		if success {
			return nil
		}
	}

	return ErrIssueUpdateFailed
}

func (c *Connector) updateIssueStateID(ctx context.Context, issueID string, stateID string) (bool, error) {
	var response struct {
		IssueUpdate *struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.client.GraphQL(ctx, issueUpdateStateMutation, map[string]any{
		"issueId": issueID,
		"stateId": stateID,
	}, &response); err != nil {
		return false, err
	}
	return response.IssueUpdate != nil && response.IssueUpdate.Success, nil
}

func (c *Connector) SetAssignee(ctx context.Context, issueID string, login string) error {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return ErrMissingIssue
	}
	login = strings.TrimSpace(login)
	if login == "" {
		return ErrMissingUser
	}

	userID, err := c.resolveUserID(ctx, login)
	if err != nil {
		if errors.Is(err, ErrMissingUser) || errors.Is(err, ErrUserNotFound) || errors.Is(err, ErrUserAmbiguous) {
			return err
		}
		return fmt.Errorf("set linear assignee: %w", err)
	}

	success, err := c.updateIssueAssigneeID(ctx, issueID, userID)
	if err != nil {
		return fmt.Errorf("set linear assignee: %w", err)
	}
	if !success {
		return ErrIssueUpdateFailed
	}
	return nil
}

func (c *Connector) updateIssueAssigneeID(ctx context.Context, issueID string, assigneeID string) (bool, error) {
	var response struct {
		IssueUpdate *struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := c.client.GraphQL(ctx, issueUpdateAssigneeMutation, map[string]any{
		"issueId":    issueID,
		"assigneeId": assigneeID,
	}, &response); err != nil {
		return false, err
	}
	return response.IssueUpdate != nil && response.IssueUpdate.Success, nil
}

func (c *Connector) SetField(context.Context, string, string, string) error {
	return connector.ErrNotImplemented
}

func (c *Connector) fetchIssueCommentsPage(ctx context.Context, issueID string, after *string) (linearCommentConnection, error) {
	var response struct {
		Issue *struct {
			Comments linearCommentConnection `json:"comments"`
		} `json:"issue"`
	}
	if err := c.client.GraphQL(ctx, issueCommentsQuery, map[string]any{
		"issueId": issueID,
		"first":   issueCommentsPageSize,
		"after":   after,
	}, &response); err != nil {
		return linearCommentConnection{}, fmt.Errorf("fetch linear issue comments: %w", err)
	}
	if response.Issue == nil {
		return linearCommentConnection{}, nil
	}
	return response.Issue.Comments, nil
}

type linearCommentConnection struct {
	Nodes    []linearComment `json:"nodes"`
	PageInfo pageInfo        `json:"pageInfo"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type linearComment struct {
	ID           string              `json:"id"`
	Body         string              `json:"body"`
	URL          string              `json:"url"`
	CreatedAt    time.Time           `json:"createdAt"`
	UpdatedAt    time.Time           `json:"updatedAt"`
	User         *linearUser         `json:"user"`
	ExternalUser *linearExternalUser `json:"externalUser"`
	BotActor     *linearBotActor     `json:"botActor"`
}

type linearUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type linearExternalUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

type linearBotActor struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	UserDisplayName string `json:"userDisplayName"`
}

func linearIssueID(issue connector.Issue) string {
	for _, value := range []string{issue.ID, issue.Identifier} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func connectorIssueComment(comment linearComment) connector.IssueComment {
	return connector.IssueComment{
		ID:                strings.TrimSpace(comment.ID),
		Backend:           connector.BackendLinear.String(),
		Body:              comment.Body,
		URL:               strings.TrimSpace(comment.URL),
		AuthorLogin:       linearAuthorLogin(comment),
		AuthorDisplayName: linearAuthorDisplayName(comment),
		CreatedAt:         linearTimePtr(comment.CreatedAt),
		UpdatedAt:         linearTimePtr(comment.UpdatedAt),
		TargetType:        connector.IssueCommentTargetIssue,
	}
}

func linearAuthorLogin(comment linearComment) string {
	if comment.User != nil {
		return firstNonBlank(comment.User.DisplayName, comment.User.Name, comment.User.ID)
	}
	if comment.ExternalUser != nil {
		return firstNonBlank(comment.ExternalUser.DisplayName, comment.ExternalUser.Name, comment.ExternalUser.ID)
	}
	if comment.BotActor != nil {
		return firstNonBlank(comment.BotActor.UserDisplayName, comment.BotActor.Name, comment.BotActor.ID)
	}
	return ""
}

func linearAuthorDisplayName(comment linearComment) string {
	if comment.User != nil {
		return firstNonBlank(comment.User.Name, comment.User.DisplayName)
	}
	if comment.ExternalUser != nil {
		return firstNonBlank(comment.ExternalUser.Name, comment.ExternalUser.DisplayName)
	}
	if comment.BotActor != nil {
		return firstNonBlank(comment.BotActor.UserDisplayName, comment.BotActor.Name)
	}
	return ""
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func linearTimePtr(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
