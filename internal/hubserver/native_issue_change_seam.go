package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemchange"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) workItemChangeService() *workitemchange.Service {
	return workitemchange.New(workitemchange.Deps{Repository: s.config.TenantRepository, PullRequestNumber: externalPullRequestNumber})
}

func (s *Service) prepareNativeIssueChangeRecord(ctx context.Context, snapshot boardProjectionSnapshot, issue *tracker.NativeIssue) error {
	return hubapi.FromFeatureError(s.workItemChangeService().Prepare(ctx, workitemchange.Snapshot{Project: snapshot.project, Item: snapshot.item, Change: snapshot.change, ChangeVersion: snapshot.changeVersion, PullRequestVersion: snapshot.pullRequestVersion}, issue))
}

func (s *Service) readNativeIssueChangePage(ctx context.Context, scope hubscope.NativeScope, issue *tracker.NativeIssue) error {
	return hubapi.FromFeatureError(s.workItemChangeService().ReadPage(ctx, s.featureScope(scope), issue))
}
