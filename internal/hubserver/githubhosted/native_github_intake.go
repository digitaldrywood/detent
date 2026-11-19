package githubhosted

import (
	"context"
	"errors"
	"strings"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githubwebhook"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type LinkedIntakeDeps struct {
	ReadProject func(context.Context, hubscope.NativeScope) (projectstore.Metadata, error)
	Importer    githubcontract.ImportBackend
	Now         func() time.Time
	SourceError func(error, time.Time) error
}

const LinkedIssueRepositoryRequired = "Read get_project_integration for this project. An administrator must associate its repository through bind_native_repository before linking GitHub issues; use source runner_checkout with an enrolled runner reporting the matching checkout."
const LinkedIssueRepositoryMismatch = "GitHub issue must belong to this native project's attached repository"

func ValidateLinkedIssueRepository(integration projectstore.Integration, repository string) error {
	if integration.Profile == "native" && integration.Repository == "" && integration.CheckoutRepository == "" {
		return hubapi.NativePublicInvalid(LinkedIssueRepositoryRequired)
	}
	if integration.Profile != "native" || (!strings.EqualFold(repository, integration.Repository) && !strings.EqualFold(repository, integration.CheckoutRepository)) {
		return hubapi.NativePublicInvalid(LinkedIssueRepositoryMismatch)
	}
	return nil
}

func (s LinkedIntakeDeps) providerError(err error, retryAt time.Time) error {
	var status *githubconnector.StatusError
	if errors.As(err, &status) {
		if status.StatusCode != 404 && status.StatusCode != 429 {
			return err
		}
	} else if !errors.Is(err, githubconnector.ErrNotFound) && !errors.Is(err, githubconnector.ErrRateLimited) {
		return err
	}
	return s.SourceError(err, retryAt)
}

func (s LinkedIntakeDeps) Fetch(ctx context.Context, scope hubscope.NativeScope, sourceURL string) (*tracker.GitHubIssueSnapshot, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(sourceURL)
	if err != nil {
		return nil, hubapi.NativeInvalid(err.Error())
	}
	project, err := s.ReadProject(ctx, scope)
	if err != nil {
		return nil, err
	}
	integration := projectstore.Integration{Profile: project.Core.Profile, Repository: project.Settings.Repository, CheckoutRepository: project.Settings.CheckoutRepository}
	if err := ValidateLinkedIssueRepository(integration, repository); err != nil {
		return nil, err
	}
	if s.Importer == nil {
		return nil, hubapi.NativePublicInvalid("GitHub App transport is unavailable for linked issue intake")
	}
	request := githubcontract.GitHubImportRequest{Profile: "native", Repository: repository, IssueNumber: number, Stage: "issue"}
	page, err := s.Importer.FetchImportPage(ctx, request)
	if err != nil {
		return nil, s.providerError(err, page.RetryAt)
	}
	if len(page.Gaps) != 0 {
		return nil, s.SourceError(hubapi.NativePublicInvalid("Source context incomplete: private access or partial page"), page.RetryAt)
	}
	if page.Issue == nil {
		return nil, hubapi.NativePublicInvalid("GitHub intake returned no issue")
	}
	fetchedURL, _, _, err := tracker.ParseGitHubIssueURL(page.Issue.URL)
	if err != nil || fetchedURL != canonical {
		return nil, hubapi.NativePublicInvalid("GitHub intake returned a different issue")
	}
	snapshot := githubwebhook.SnapshotFromSource(*page.Issue, s.Now())
	snapshot.URL = canonical
	request.Stage = "comments"
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages == 500 {
			return nil, hubapi.NativePublicInvalid("Linked issue discussion exceeds the intake page bound")
		}
		page, err = s.Importer.FetchImportPage(ctx, request)
		if err != nil {
			return nil, s.providerError(err, page.RetryAt)
		}
		if len(page.Gaps) != 0 {
			return nil, s.SourceError(hubapi.NativePublicInvalid("Source context incomplete: private access or partial page"), page.RetryAt)
		}
		for _, record := range page.Records {
			if record.Kind == "comment" && !strings.Contains(record.Body, "<!-- detent-summary:") && !seen[record.Provenance.ExternalID] {
				snapshot.Comments = append(snapshot.Comments, tracker.GitHubIssueComment{Body: record.Body, Provenance: record.Provenance})
				seen[record.Provenance.ExternalID] = true
			}
		}
		if len(snapshot.Comments) > 1000 {
			return nil, hubapi.NativePublicInvalid("Linked issue discussion exceeds the intake bound")
		}
		if page.NextCursor == "" {
			break
		}
		if seen["cursor:"+page.NextCursor] {
			return nil, hubapi.NativePublicInvalid("Linked issue discussion exceeds the intake bound")
		}
		seen["cursor:"+page.NextCursor] = true
		request.Cursor = page.NextCursor
	}
	return &snapshot, nil
}
