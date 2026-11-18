package workitemchange

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// A work item's change review surface.
//
// A completed item is only ever moved out of its active lane by the
// orchestrator, and the orchestrator's readiness rule was a pull request and
// nothing else: every gate kind but artifact required the issue to carry an
// open pull request. A hub-native item never has one, so a native project's
// completed item was never judged ready and the promotion was never even
// attempted.
//
// This is what a native item is judged on instead. Two facts, and the first
// has to be stated rather than inferred from a missing pull request: a project
// with no GitHub connector can never produce one (the pull request contract, "projects without a
// GitHub connector show the change request alone"), while a project that has a
// connector may simply not have opened the pull request yet. Those are
// different situations and a promotion decides them differently.
//
// The surface is assembled only for a caller that asks for it, because it
// costs three reads the rest of the work item resource does not need.

func (s *Service) Prepare(snapshot Snapshot, issue *tracker.NativeIssue) {
	change := &tracker.NativeIssueChange{Connector: tracker.NativeChangeConnectorNone}
	if snapshot.Project.Settings.RepositoryEnabled {
		change.Connector = tracker.NativeChangeConnectorGitHub
	}
	issue.Change = change
	issue.PullRequest = nil
	issue.ListChange = nil
	if retained := snapshot.PullRequestVersion; retained != nil {
		if number := s.deps.PullRequestNumber(retained.External); number > 0 {
			issue.PullRequest = &tracker.NativePullRequest{Number: number, URL: retained.External.URL}
		}
	}
	version := snapshot.ChangeVersion
	var external tracker.ChangeExternalReference
	if version != nil {
		external = tracker.ChangeExternalReference{Provider: version.ExternalProvider, ID: version.ExternalID, URL: version.ExternalURL}
	}
	published := false
	if publication := snapshot.Publication; version != nil && publication != nil && publication.ChangeID == version.ChangeID && publication.HeadSHA == version.HeadSHA && publication.Repository == version.Repository && publication.PolicyID == version.PolicyRevisionID && publication.SourceAttemptID == version.AttemptID {
		external = publication.External
		published = true
	}
	if attempt, diff := snapshot.Attempt, snapshot.Diff; attempt != nil && attempt.Status == "succeeded" && diff != nil && (version == nil || diff.HeadSHA == version.HeadSHA) && diff.Generation.Seq == attempt.Sequence && diff.FileCount > 0 {
		change.Revision = tracker.Revision(attempt.WorkItemRevision)
	}
	if snapshot.Change == nil {
		return
	}
	change.ChangeID, change.State = snapshot.Change.ID, "open"
	if snapshot.Change.LandedVersionID != nil && snapshot.Change.CurrentVersionID != nil && *snapshot.Change.LandedVersionID == *snapshot.Change.CurrentVersionID {
		change.State = "closed"
	}
	if version == nil {
		return
	}
	issue.ListChange = &tracker.NativeListChange{HeadSHA: version.HeadSHA}
	if snapshot.Change.LandedVersionID != nil && snapshot.Change.CurrentVersionID != nil && *snapshot.Change.LandedVersionID == *snapshot.Change.CurrentVersionID && version.ID == *snapshot.Change.CurrentVersionID {
		issue.ListChange.Branch = hubapi.StringValue(snapshot.Change.LandedBaseRef)
		issue.ListChange.HeadSHA = hubapi.StringValue(snapshot.Change.LandedMergeSHA)
	}
	change.HeadSHA = version.HeadSHA

	change.URL, change.Number = external.URL, s.deps.PullRequestNumber(external)
	if external.Provider == "github" {
		change.Connector = tracker.NativeChangeConnectorGitHub
	}
	if published && change.Number > 0 {
		issue.PullRequest = &tracker.NativePullRequest{Number: change.Number, URL: change.URL}
	}
}

func (s *Service) ReadPage(ctx context.Context, scope hubapi.Scope, issue *tracker.NativeIssue) error {
	return s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.BoardReader)
		if !ok {
			return hubapi.WorkItemStorageUnavailable()
		}
		page, err := reader.BoardItem(ctx, scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if page.PageIssue == nil || page.SourceRevision != int64(issue.Revision) {
			return hubapi.WorkItemStorageUnavailable()
		}
		if scope.HostedRole == "reporter" && (scope.Actor.PrincipalID == "" || page.Display.ChangePrincipalID != scope.Actor.PrincipalID) {
			return nil
		}
		issue.Change, issue.ListChange, issue.PullRequest = page.PageIssue.Change, page.PageIssue.ListChange, page.PageIssue.PullRequest
		return nil
	})
}
