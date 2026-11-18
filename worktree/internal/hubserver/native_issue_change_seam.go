package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemchange"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) workItemChangeService() *workitemchange.Service {
	return workitemchange.New(workitemchange.Deps{Repository: s.config.TenantRepository, PullRequestNumber: externalPullRequestNumber})
}

func (s *Service) prepareNativeIssueChangeRecord(snapshot boardProjectionSnapshot, issue *tracker.NativeIssue) {
	var attempt *leasestore.Attempt
	if snapshot.attempt != nil {
		attempt = &snapshot.attempt.Attempt
	}
	s.workItemChangeService().Prepare(workitemchange.Snapshot{
		Project: snapshot.project, Change: snapshot.change, ChangeVersion: snapshot.changeVersion,
		PullRequestVersion: snapshot.pullRequestVersion, Publication: snapshot.publication, Attempt: attempt, Diff: snapshot.diff,
	}, issue)
}

func (s *Service) readNativeIssueChangePage(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	return hubapi.FromFeatureError(s.workItemChangeService().ReadPage(ctx, s.authorization().Scope(scope), issue))
}
