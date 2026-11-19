package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/githubhosted"
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

func (s *Service) fetchLinkedSnapshot(ctx context.Context, scope hubscope.NativeScope, sourceURL string) (*tracker.GitHubIssueSnapshot, error) {
	deps := githubhosted.LinkedIntakeDeps{
		ReadProject: s.readProjectMetadata, Importer: s.config.ImportBackend, Now: s.config.Clock,
		SourceError: func(err error, retryAt time.Time) error {
			return &linkedSnapshotSourceError{err: err, retryAt: retryAt}
		},
	}
	return deps.Fetch(ctx, scope, sourceURL)
}
