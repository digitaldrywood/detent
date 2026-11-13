package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationPreferenceIssue struct {
	plan    workItemRecordPlan
	fields  workItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	search  searchstore.Document
	changed bool
}

func (s *Service) prepareConversationPreferenceIssue(ctx context.Context, scope nativeScope, record conversationRecord, preferences conversation.Preferences) (conversationPreferenceIssue, error) {
	var result conversationPreferenceIssue
	if record.WorkItemID == "" {
		return result, nil
	}
	plan, err := s.loadWorkItemPlan(ctx, scope, record.WorkItemID, "", tracker.UpdateIssue{})
	if err != nil {
		return result, err
	}
	result.plan = plan
	body := conversationAgentOverrideBody(plan.issue.Body, preferences)
	if body == plan.issue.Body {
		return result, nil
	}
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return s.authorizeWorkItemRecordScope(ctx, reader, scope)
	}); err != nil {
		return result, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	plan.issue.Body = body
	plan.issue.Revision++
	plan.issue.UpdatedAt, plan.issue.LastActivityAt = now, now
	plan.fields = []string{"body"}
	result.plan, result.changed = plan, true
	result.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(plan.issue)
	if err != nil {
		return result, err
	}
	content := eventstore.NewContent(s.runDetailStore())
	result.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, RecordID: record.WorkItemID, Revision: int64(plan.issue.Revision)}, raw)
	if err != nil {
		return result, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: plan.issue.WorkItemID, Type: "issue.edited", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: plan.issue.Revision, Fields: plan.fields}})
	if err != nil {
		return result, err
	}
	result.event, err = content.PrepareEvent(ctx, scope.organization, raw)
	if err != nil {
		return result, err
	}
	result.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: plan.issue.WorkItemID, ID: "issue:" + record.WorkItemID, SourceID: record.WorkItemID, Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: now}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
	return result, err
}

func (p conversationPreferenceIssue) Lock(ctx context.Context, writer tenantstore.Writer) error {
	if p.plan.current.ID == 0 {
		return nil
	}
	project, err := writer.ProjectRecord(ctx, p.plan.current.ProjectID)
	if err != nil {
		return err
	}
	if !sameWorkItemProjectPlan(p.plan.project, project) {
		return corestore.ErrSerializationFailure
	}
	current, err := writer.LockWorkItem(ctx, p.plan.current.ProjectID, p.plan.current.NativeID)
	if err != nil {
		return err
	}
	if current.Revision != p.plan.current.Revision {
		return nativeConflict(tracker.Revision(current.Revision))
	}
	return nil
}

func (p conversationPreferenceIssue) Write(ctx context.Context, writer tenantstore.Writer) error {
	if !p.changed {
		return nil
	}
	if err := writeWorkItemRecord(ctx, writer, p.plan, p.fields); err != nil {
		return err
	}
	source, err := writer.LinkedSource(ctx, p.plan.current.ProjectID, p.plan.current.NativeID)
	if err == nil {
		if source.SnapshotSHA256 != "" {
			source.ExpectedSnapshotSHA256 = new(source.SnapshotSHA256)
		}
		source.BodyOwned = true
		if _, err := writer.UpdateLinkedSource(ctx, source); err != nil {
			return err
		}
	} else if !errors.Is(err, corestore.ErrNotFound) {
		return err
	}
	if _, err := writer.CreateCollaborationVersion(ctx, p.version); err != nil {
		return err
	}
	if err := writer.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: p.version.OrganizationID, ProjectID: p.version.ProjectID, WorkItemID: p.version.WorkItemID, Revision: p.version.Revision, VersionSHA256: p.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(p.plan.issue.Body)}); err != nil {
		return err
	}
	if _, err := writer.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	if err := writer.PutSearch(ctx, p.search, p.plan.current.Revision); err != nil {
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return writer.PutSearch(ctx, p.search, 0)
	}
	return nil
}

func (p conversationPreferenceIssue) Changed() bool { return p.changed }
