package github

import (
	"context"

	"fmt"

	"net/http"
	"strings"
)

func (c *Connector) CreatePullRequestComment(ctx context.Context, repository string, number int, body string) error {
	owner, name, ok := splitRepositoryName(repository)
	if !ok || number <= 0 {
		return ErrCommentCreateFailed
	}

	var response struct {
		NodeID string `json:"node_id"`
	}
	ref := issueRef{Owner: owner, Name: name, Number: number}
	body = c.protectPublicationText(ctx, repository, body)
	if err := c.client.REST(ctx, http.MethodPost, restIssueCommentsPath(ref), map[string]any{"body": body}, &response); err != nil {
		return fmt.Errorf("create github pull request comment: %w", err)
	}
	if strings.TrimSpace(response.NodeID) == "" {
		return ErrCommentCreateFailed
	}

	return nil
}

func actorLogin(actor *actor) string {
	if actor == nil {
		return ""
	}
	return strings.TrimSpace(actor.Login)
}
