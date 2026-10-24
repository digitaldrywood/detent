package web

import (
	"context"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func readIssueDiscussion(ctx context.Context, source connector.Connector, issue connector.Issue) ([]connector.IssueComment, error) {
	reader, ok := source.(connector.IssueCommentReader)
	if !ok {
		return nil, operatortool.ErrReadUnavailable
	}
	comments, err := reader.FetchIssueComments(ctx, issue)
	if err != nil {
		return nil, operatortool.ErrReadUnavailable
	}
	return comments, nil
}

func readPRDiscussion(ctx context.Context, source connector.Connector, repository string, number int) ([]connector.IssueComment, error) {
	reader, ok := source.(connector.PullRequestCommentReader)
	if !ok || repository == "" || number <= 0 {
		return nil, operatortool.ErrReadUnavailable
	}
	comments, err := reader.FetchPullRequestComments(ctx, repository, number)
	if err != nil {
		return nil, operatortool.ErrReadUnavailable
	}
	return comments, nil
}
