package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type linkedSourceURLKey struct{}

func linkedSourceNumber(source string) int {
	_, _, number, err := tracker.ParseGitHubIssueURL(source)
	if err != nil {
		return 0
	}
	return number
}

type linkedSnapshotSourceError struct {
	err     error
	retryAt time.Time
}

func (failure *linkedSnapshotSourceError) Error() string { return failure.err.Error() }
func (failure *linkedSnapshotSourceError) Unwrap() error { return failure.err }

func linkedSnapshotProviderError(err error, retryAt time.Time) error {
	var status *githubconnector.StatusError
	if errors.As(err, &status) {
		if status.StatusCode != 404 && status.StatusCode != 429 {
			return err
		}
	} else if !errors.Is(err, githubconnector.ErrNotFound) && !errors.Is(err, githubconnector.ErrRateLimited) {
		return err
	}
	return &linkedSnapshotSourceError{err: err, retryAt: retryAt}
}

func (s *Service) fetchLinkedSnapshot(ctx context.Context, scope hubscope.NativeScope, sourceURL string) (*tracker.GitHubIssueSnapshot, error) {
	canonical, repository, number, err := tracker.ParseGitHubIssueURL(sourceURL)
	if err != nil {
		return nil, hubapi.NativeInvalid(err.Error())
	}
	project, err := s.readProjectRecord(ctx, scope)
	integration := projectIntegrationRecord(project)
	if err != nil {
		return nil, err
	}
	if err := validateLinkedIssueRepository(integration, repository); err != nil {
		return nil, err
	}
	if s.config.ImportBackend == nil {
		return nil, hubapi.NativePublicInvalid("GitHub App transport is unavailable for linked issue intake")
	}
	request := GitHubImportRequest{Profile: "native", Repository: repository, IssueNumber: number, Stage: "issue"}
	page, err := s.config.ImportBackend.FetchImportPage(ctx, request)
	if err != nil {
		return nil, linkedSnapshotProviderError(err, page.RetryAt)
	}
	if len(page.Gaps) != 0 {
		return nil, &linkedSnapshotSourceError{err: hubapi.NativePublicInvalid("Source context incomplete: private access or partial page"), retryAt: page.RetryAt}
	}
	if page.Issue == nil {
		return nil, hubapi.NativePublicInvalid("GitHub intake returned no issue")
	}
	fetchedURL, _, _, err := tracker.ParseGitHubIssueURL(page.Issue.URL)
	if err != nil || fetchedURL != canonical {
		return nil, hubapi.NativePublicInvalid("GitHub intake returned a different issue")
	}
	snapshot := linkedSnapshotFromSource(*page.Issue, s.config.now())
	snapshot.URL = canonical
	request.Stage = "comments"
	seen := map[string]bool{}
	for pages := 0; ; pages++ {
		if pages == 500 {
			return nil, hubapi.NativePublicInvalid("Linked issue discussion exceeds the intake page bound")
		}
		page, err = s.config.ImportBackend.FetchImportPage(ctx, request)
		if err != nil {
			return nil, linkedSnapshotProviderError(err, page.RetryAt)
		}
		if len(page.Gaps) != 0 {
			return nil, &linkedSnapshotSourceError{err: hubapi.NativePublicInvalid("Source context incomplete: private access or partial page"), retryAt: page.RetryAt}
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
