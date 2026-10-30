package github

import (
	"context"

	"fmt"

	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
)

func (c *Connector) fetchIssueComments(ctx context.Context, ref issueRef) ([]issueComment, error) {
	response, err := fetchRESTList[restComment](ctx, c.client, restIssueCommentsListPath(ref))
	if err != nil {
		return nil, err
	}
	comments := make([]issueComment, 0, len(response))
	for _, comment := range response {
		comments = append(comments, issueComment{
			ID:                restCommentID(comment),
			Body:              comment.Body,
			URL:               comment.HTMLURL,
			Author:            comment.User,
			AuthorAssociation: comment.AuthorAssociation,
			CreatedAt:         restTimeString(comment.CreatedAt),
			UpdatedAt:         restTimeString(comment.UpdatedAt),
		})
	}
	return comments, nil
}

func (c *Connector) FetchPullRequestComments(ctx context.Context, repository string, number int) ([]connector.IssueComment, error) {
	owner, name, ok := splitRepositoryName(repository)
	if !ok || number <= 0 {
		return []connector.IssueComment{}, nil
	}

	comments, err := c.fetchIssueComments(ctx, issueRef{Owner: owner, Name: name, Number: number})
	if err != nil {
		return nil, fmt.Errorf("fetch github pull request comments: %w", err)
	}
	out := make([]connector.IssueComment, 0, len(comments))
	for _, comment := range comments {
		out = append(out, connectorIssueComment(comment, connector.IssueCommentTargetPullRequest))
	}
	return out, nil
}

func connectorIssueComment(comment issueComment, targetType string) connector.IssueComment {
	return connector.IssueComment{
		ID:               strings.TrimSpace(comment.ID),
		Backend:          connector.BackendGitHub.String(),
		Body:             comment.Body,
		URL:              comment.URL,
		AuthorLogin:      actorLogin(comment.Author),
		AuthorKind:       actorType(comment.Author),
		AuthorAuthorized: githubAuthorCanOperate(comment.AuthorAssociation),
		CreatedAt:        parseGitHubTime(comment.CreatedAt),
		UpdatedAt:        parseGitHubTime(comment.UpdatedAt),
		TargetType:       targetType,
	}
}

func githubAuthorCanOperate(association string) bool {
	switch strings.ToUpper(strings.TrimSpace(association)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}
