package api

import (
	"context"
	"net/http"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) IntakeSource(ctx context.Context, id tracker.NativeWorkItemID, request tracker.GitHubIntake) (tracker.NativeIssue, error) {
	var issue tracker.NativeIssue
	path, err := NativeItemPath(id)
	if err != nil {
		return issue, err
	}
	err = c.client.request(ctx, http.MethodPost, c.base()+path+"/source-intake", request, &issue)
	return issue, err
}
