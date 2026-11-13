package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) retainClosedWorkItems(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	var after corestore.WorkItemRetentionCursor
	var page corestore.WorkItemRetentionPage
	if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(corestore.WorkItemRetentionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		after, err = owner.WorkItemRetentionCursor(ctx)
		if err != nil {
			return err
		}
		page, err = owner.WorkItemRetentionCandidates(ctx, now, after, 4)
		return err
	}); err != nil {
		return err
	}

	advance := func(next corestore.WorkItemRetentionCursor) (bool, error) {
		checkpoint, cancel := context.WithTimeout(context.WithoutCancel(ctx), hubapi.ReadTimeout)
		defer cancel()
		changed := false
		err := s.config.TenantRepository.WriteTenant(checkpoint, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(corestore.WorkItemRetentionWriter)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			changed, err = owner.AdvanceWorkItemRetention(checkpoint, after, next)
			return err
		})
		if err == nil && changed {
			after = next
		}
		return changed, err
	}
	var failures error
	for _, candidate := range page.Items {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		if err := s.retainClosedWorkItem(ctx, organization, candidate, now); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !nativeRetentionItemFailure(err) {
				return errors.Join(failures, err)
			}
			failures = errors.Join(failures, fmt.Errorf("archive closed issue: %w", err))
		}
		changed, err := advance(candidate.Cursor)
		if err != nil {
			return errors.Join(failures, err)
		}
		if !changed {
			return failures
		}
	}
	if page.Next.ID == 0 && (len(page.Items) > 0 || after.ID != 0) {
		_, err := advance(corestore.WorkItemRetentionCursor{})
		return errors.Join(failures, err)
	}
	return failures
}

func nativeRetentionItemFailure(err error) bool {
	return errors.Is(err, artifact.ErrMissing) || errors.Is(err, artifact.ErrIntegrity) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, attachment.ErrInvalid) || errors.Is(err, attachment.ErrTooLarge) || errors.Is(err, eventstore.ErrInvalidRecord)
}

func (s *Service) retainClosedWorkItem(ctx context.Context, organization tracker.OrganizationID, candidate corestore.WorkItemRetentionCandidate, now time.Time) error {
	scope := hubscope.NativeScope{Organization: organization, Project: candidate.ProjectID}
	input := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: tracker.Revision(candidate.Revision)}, Archived: true}
	plan, err := s.loadWorkItemPlan(ctx, scope, string(candidate.WorkItemID), "", input)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if plan.Current.Archived || plan.Current.Revision != candidate.Revision || plan.Current.EventSequence != candidate.EventSequence {
		return nil
	}
	var days int64
	var eligible bool
	if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(corestore.WorkItemRetentionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		days, eligible, err = owner.WorkItemRetentionPolicy(ctx, plan.Current, plan.Project.Settings.Revision, now)
		return err
	}); err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	plan, err = s.planWorkItemRecord(ctx, scope, plan, input)
	if err != nil {
		return err
	}
	fields, err := s.prepareWorkItemFields(ctx, organization, plan)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(plan.Issue)
	if err != nil {
		return err
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: scope.Project, WorkItemID: candidate.WorkItemID, RecordID: string(candidate.WorkItemID), Revision: int64(plan.Issue.Revision)}, raw)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: organization, ProjectID: scope.Project, AggregateID: candidate.WorkItemID, ID: hubapi.NewID("evt"), Type: plan.EventType, SchemaVersion: 1, Actor: tracker.Actor{Kind: "integration", PrincipalID: "auto_archive"}, RecordedAt: plan.Issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, Fields: plan.Fields, Operation: "archive", ReasonDetail: fmt.Sprintf("Archived automatically after %d days", days)}})
	if err != nil {
		return err
	}
	event, err := content.PrepareEvent(ctx, organization, raw)
	if err != nil {
		return err
	}
	search, err := searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: organization, ProjectID: scope.Project, WorkItemID: candidate.WorkItemID, ID: "issue:" + string(candidate.WorkItemID), SourceID: string(candidate.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(plan.Issue.Revision), UpdatedAt: plan.Issue.UpdatedAt}, searchstore.Text{Title: plan.Issue.Title, Body: plan.Issue.Body, Labels: plan.Issue.Labels})
	if err != nil {
		return err
	}
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if !hubrecords.SameWorkItemProjectPlan(plan.Project, project) {
			return nil
		}
		current, err := w.LockWorkItem(ctx, scope.Project, candidate.WorkItemID)
		if err != nil {
			return err
		}
		owner, ok := w.(corestore.WorkItemRetentionWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		currentDays, eligible, err := owner.WorkItemRetentionPolicy(ctx, current, project.Settings.Revision, now)
		if err != nil {
			return err
		}
		if current.Revision != plan.Current.Revision || current.EventSequence != plan.Current.EventSequence || !eligible || currentDays != days {
			return nil
		}
		if err := hubrecords.WriteWorkItemRecord(ctx, w, plan, fields); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		if err := w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: organization, ProjectID: scope.Project, WorkItemID: candidate.WorkItemID, Revision: version.Revision, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(plan.Issue.Body)}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		return w.PutSearch(ctx, search, plan.Current.Revision)
	})
}
