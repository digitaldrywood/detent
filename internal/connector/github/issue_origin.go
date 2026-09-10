package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func (c *Connector) findOpenFingerprint(ctx context.Context, fingerprint string) (connector.Issue, bool, error) {
	for page := 1; ; page++ {
		var issues []restIssue
		path := fmt.Sprintf("/repos/%s/%s/issues?state=open&per_page=100&page=%d", c.repository.Owner, c.repository.Name, page)
		if err := c.client.REST(ctx, http.MethodGet, path, nil, &issues); err != nil {
			return connector.Issue{}, false, fmt.Errorf("find open issue fingerprint: %w", err)
		}
		for _, issue := range issues {
			if issue.PullRequest != nil || issue.Body == nil || issue.State == "closed" {
				continue
			}
			origin, ok := issueorigin.Parse(*issue.Body)
			if !ok || origin.Fingerprint != fingerprint {
				continue
			}
			ref := issueRef{Owner: c.repository.Owner, Name: c.repository.Name, Number: issue.Number}
			node := githubIssueNodeFromREST(ref, issue)
			c.cacheIssueRef(node)
			return c.buildLabelIssue(node, c.labelStatusFromLabels(node.Labels)), true, nil
		}
		if len(issues) < 100 {
			return connector.Issue{}, false, nil
		}
	}
}
