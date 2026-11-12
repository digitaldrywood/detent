package hubserver

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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

func (s *Service) prepareNativeIssueChangeRecord(ctx context.Context, snapshot boardProjectionSnapshot, issue *tracker.NativeIssue) error {
	change := &tracker.NativeIssueChange{Connector: tracker.NativeChangeConnectorNone}
	if snapshot.project.Settings.RepositoryEnabled {
		change.Connector = tracker.NativeChangeConnectorGitHub
	}
	issue.Change = change
	issue.PullRequest = nil
	issue.ListChange = nil
	if retained := snapshot.pullRequestVersion; retained != nil {
		if number := externalPullRequestNumber(retained.External); number > 0 {
			issue.PullRequest = &tracker.NativePullRequest{Number: number, URL: retained.External.URL}
		}
	}
	version := snapshot.changeVersion
	var external tracker.ChangeExternalReference
	if version != nil {
		external = tracker.ChangeExternalReference{Provider: version.ExternalProvider, ID: version.ExternalID, URL: version.ExternalURL}
	}
	published := false
	err := s.config.TenantRepository.ReadTenant(ctx, snapshot.item.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
		if version != nil {
			publication, err := r.PRPublication(ctx, version.ID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			if err == nil && publication.ChangeID == version.ChangeID && publication.HeadSHA == version.HeadSHA && publication.Repository == version.Repository && publication.PolicyID == version.PolicyRevisionID && publication.SourceAttemptID == version.AttemptID {
				external = publication.External
				published = true
			}
		}
		attempt, err := r.LatestAttempt(ctx, snapshot.item.ID)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if attempt.Status != "succeeded" {
			return nil
		}
		diff, err := r.LatestAttemptDiff(ctx, snapshot.item.ProjectID, attempt.ID, "attempt")
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if (version == nil || diff.HeadSHA == version.HeadSHA) && diff.Generation.Seq == attempt.Sequence && diff.FileCount > 0 {
			change.Revision = tracker.Revision(attempt.WorkItemRevision)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if snapshot.change == nil {
		return nil
	}
	change.ChangeID, change.State = snapshot.change.ID, "open"
	if snapshot.change.LandedVersionID != nil && snapshot.change.CurrentVersionID != nil && *snapshot.change.LandedVersionID == *snapshot.change.CurrentVersionID {
		change.State = "closed"
	}
	if version == nil {
		return nil
	}
	issue.ListChange = &tracker.NativeListChange{HeadSHA: version.HeadSHA}
	if snapshot.change.LandedVersionID != nil && snapshot.change.CurrentVersionID != nil && *snapshot.change.LandedVersionID == *snapshot.change.CurrentVersionID && version.ID == *snapshot.change.CurrentVersionID {
		issue.ListChange.Branch = stringValue(snapshot.change.LandedBaseRef)
		issue.ListChange.HeadSHA = stringValue(snapshot.change.LandedMergeSHA)
	}
	change.HeadSHA = version.HeadSHA

	change.URL, change.Number = external.URL, externalPullRequestNumber(external)
	if external.Provider == "github" {
		change.Connector = tracker.NativeChangeConnectorGitHub
	}
	if published && change.Number > 0 {
		issue.PullRequest = &tracker.NativePullRequest{Number: change.Number, URL: change.URL}
	}
	return nil
}

func (s *Service) readNativeIssueChangePage(ctx context.Context, scope nativeScope, issue *tracker.NativeIssue) error {
	return s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(viewstore.BoardReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		page, err := reader.BoardItem(ctx, scope.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if page.PageIssue == nil || page.SourceRevision != int64(issue.Revision) {
			return workItemStorageUnavailable()
		}
		if !scope.reporterOwns(tracker.Actor{PrincipalID: page.Display.ChangePrincipalID}) {
			return nil
		}
		issue.Change, issue.ListChange, issue.PullRequest = page.PageIssue.Change, page.PageIssue.ListChange, page.PageIssue.PullRequest
		return nil
	})
}
