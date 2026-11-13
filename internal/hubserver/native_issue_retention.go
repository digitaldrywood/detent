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
	scope := nativeScope{organization: organization, project: candidate.ProjectID}
	input := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: tracker.Revision(candidate.Revision)}, Archived: true}
	plan, err := s.loadWorkItemPlan(ctx, scope, string(candidate.WorkItemID), "", input)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if plan.current.Archived || plan.current.Revision != candidate.Revision || plan.current.EventSequence != candidate.EventSequence {
		return nil
	}
	var days int64
	var eligible bool
	if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(corestore.WorkItemRetentionReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		days, eligible, err = owner.WorkItemRetentionPolicy(ctx, plan.current, plan.project.Settings.Revision, now)
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
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		return err
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: scope.project, WorkItemID: candidate.WorkItemID, RecordID: string(candidate.WorkItemID), Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		return err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{OrganizationID: organization, ProjectID: scope.project, AggregateID: candidate.WorkItemID, ID: hubapi.NewID("evt"), Type: plan.eventType, SchemaVersion: 1, Actor: tracker.Actor{Kind: "integration", PrincipalID: "auto_archive"}, RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields, Operation: "archive", ReasonDetail: fmt.Sprintf("Archived automatically after %d days", days)}})
	if err != nil {
		return err
	}
	event, err := content.PrepareEvent(ctx, organization, raw)
	if err != nil {
		return err
	}
	search, err := searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: organization, ProjectID: scope.project, WorkItemID: candidate.WorkItemID, ID: "issue:" + string(candidate.WorkItemID), SourceID: string(candidate.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: plan.issue.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
	if err != nil {
		return err
	}
	return s.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if !sameWorkItemProjectPlan(plan.project, project) {
			return nil
		}
		current, err := w.LockWorkItem(ctx, scope.project, candidate.WorkItemID)
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
		if current.Revision != plan.current.Revision || current.EventSequence != plan.current.EventSequence || !eligible || currentDays != days {
			return nil
		}
		if err := writeWorkItemRecord(ctx, w, plan, fields); err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(ctx, version); err != nil {
			return err
		}
		if err := w.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: organization, ProjectID: scope.project, WorkItemID: candidate.WorkItemID, Revision: version.Revision, VersionSHA256: version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(plan.issue.Body)}); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, event); err != nil {
			return err
		}
		return w.PutSearch(ctx, search, plan.current.Revision)
	})
}
