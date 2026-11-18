package githubfake

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Backend struct {
	Snapshot func(hubserver.GitHubImportRequest) tracker.GitHubIssueSnapshot
	Steps    chan struct{}
}

var updatedAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

func (b Backend) Reconcile(ctx context.Context, request hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	if request.Mode != "" && b.Steps != nil {
		select {
		case b.Steps <- struct{}{}:
		case <-ctx.Done():
			return hubserver.ReconcileSnapshot{}, ctx.Err()
		}
	}
	return hubserver.ReconcileSnapshot{Repository: hubserver.RepositorySource{NodeID: "R_source", Owner: "example-org", Name: "example-project", UpdatedAt: time.Now().UTC()}}, nil
}

func (b Backend) FetchImportPage(_ context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	snapshot := b.Snapshot(request)
	if request.Stage == "issue" {
		return hubserver.GitHubImportPage{Issue: &hubserver.IssueSource{NodeID: snapshot.Provenance.ExternalID, Number: request.IssueNumber, URL: snapshot.URL, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
	}
	page := hubserver.GitHubImportPage{}
	for _, comment := range snapshot.Comments {
		page.Records = append(page.Records, hubserver.GitHubImportRecord{Kind: "comment", Body: comment.Body, Provenance: comment.Provenance})
	}
	return page, nil
}

func Issue(body string) tracker.GitHubIssueSnapshot {
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "source-author", CreatedAt: updatedAt.Add(-time.Hour), UpdatedAt: updatedAt, ObservedAt: updatedAt}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/example-org/example-project/issues/12", Title: "Imported execution task", Body: body, Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Useful source discussion", Provenance: comment}}}
}
