package scenariotest

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type intakeRepositoryBackend struct {
	snapshot func(hubserver.GitHubImportRequest) tracker.GitHubIssueSnapshot
}

func (intakeRepositoryBackend) Reconcile(context.Context, hubserver.ReconcileRequest) (hubserver.ReconcileSnapshot, error) {
	return hubserver.ReconcileSnapshot{Repository: hubserver.RepositorySource{NodeID: "R_source", Owner: "example-org", Name: "example-project", UpdatedAt: time.Now().UTC()}}, nil
}

func (b intakeRepositoryBackend) FetchImportPage(_ context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	snapshot := intakeSnapshot()
	if b.snapshot != nil {
		snapshot = b.snapshot(request)
	}
	if request.Stage == "issue" {
		return hubserver.GitHubImportPage{Issue: &hubserver.IssueSource{NodeID: snapshot.Provenance.ExternalID, Number: request.IssueNumber, URL: snapshot.URL, Title: snapshot.Title, Body: snapshot.Body, AuthorID: snapshot.Provenance.AuthorID, CreatedAt: snapshot.Provenance.CreatedAt, UpdatedAt: snapshot.Provenance.UpdatedAt}}, nil
	}
	page := hubserver.GitHubImportPage{}
	for _, comment := range snapshot.Comments {
		page.Records = append(page.Records, hubserver.GitHubImportRecord{Kind: "comment", Body: comment.Body, Provenance: comment.Provenance})
	}
	return page, nil
}

func intakeSnapshot() tracker.GitHubIssueSnapshot {
	now := time.Now().UTC()
	prov := tracker.Provenance{Provider: "github", ExternalID: "I_source", AuthorID: "source-author", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Minute), ObservedAt: now}
	comment := prov
	comment.ExternalID = "C_source"
	return tracker.GitHubIssueSnapshot{URL: "https://github.com/example-org/example-project/issues/12", Title: "Imported execution task", Body: "Complete implementation context" + issueContractTestSections, Provenance: prov, Comments: []tracker.GitHubIssueComment{{Body: "Useful source discussion", Provenance: comment}}}
}
