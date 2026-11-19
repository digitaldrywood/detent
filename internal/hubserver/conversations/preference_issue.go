package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type PreparedPreferenceIssue struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	event   eventstore.Event
	changed bool
}

func (s PreferenceIssueDeps) PrepareConversationPreferenceIssue(ctx context.Context, scope hubscope.NativeScope, record ConversationRecord, preferences conversation.Preferences) (PreparedPreferenceIssue, error) {
	var result PreparedPreferenceIssue
	if record.WorkItemID == "" {
		return result, nil
	}
	plan, err := s.LoadPlan(ctx, scope, record.WorkItemID, "", tracker.UpdateIssue{})
	if err != nil {
		return result, err
	}
	result.plan = plan
	body := ConversationAgentOverrideBody(plan.Issue.Body, preferences)
	if body == plan.Issue.Body {
		return result, nil
	}
	if err := s.Authorize(ctx, scope); err != nil {
		return result, err
	}
	now := s.Now().UTC().Truncate(time.Microsecond)
	plan.Issue.Body = body
	plan.Issue.Revision++
	plan.Issue.UpdatedAt, plan.Issue.LastActivityAt = now, now
	plan.Fields = []string{"body"}
	result.plan, result.changed = plan, true
	result.fields, err = s.PrepareFields(ctx, scope.Organization, plan)
	if err != nil {
		return result, err
	}
	raw, err := json.Marshal(plan.Issue)
	if err != nil {
		return result, err
	}
	content := eventstore.NewContent(s.Details())
	result.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: record.WorkItemID, Revision: int64(plan.Issue.Revision)}, raw)
	if err != nil {
		return result, err
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, Type: "issue.edited", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: now, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, Fields: plan.Fields}})
	if err != nil {
		return result, err
	}
	result.event, err = content.PrepareEvent(ctx, scope.Organization, raw)
	if err != nil {
		return result, err
	}
	return result, err
}

func (p PreparedPreferenceIssue) Lock(ctx context.Context, writer tenantstore.Writer) error {
	if p.plan.Current.ID == 0 {
		return nil
	}
	project, err := writer.ProjectRecord(ctx, p.plan.Current.ProjectID)
	if err != nil {
		return err
	}
	if !hubrecords.SameWorkItemProjectPlan(p.plan.Project, project) {
		return corestore.ErrSerializationFailure
	}
	current, err := writer.LockWorkItem(ctx, p.plan.Current.ProjectID, p.plan.Current.NativeID)
	if err != nil {
		return err
	}
	if current.Revision != p.plan.Current.Revision {
		return hubapi.NativeConflict(tracker.Revision(current.Revision))
	}
	return nil
}

func (p PreparedPreferenceIssue) Write(ctx context.Context, writer tenantstore.Writer) error {
	if !p.changed {
		return nil
	}
	if err := hubrecords.WriteWorkItemRecord(ctx, writer, p.plan, p.fields); err != nil {
		return err
	}
	source, err := writer.LinkedSource(ctx, p.plan.Current.ProjectID, p.plan.Current.NativeID)
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
	if err := writer.BindDoctorProposalIdentities(ctx, postgres.DoctorProposalIdentityBinding{OrganizationID: p.version.OrganizationID, ProjectID: p.version.ProjectID, WorkItemID: p.version.WorkItemID, Revision: p.version.Revision, VersionSHA256: p.version.PayloadSHA256, Identities: postgres.DeriveDoctorProposalIdentities(p.plan.Issue.Body)}); err != nil {
		return err
	}
	if _, err := writer.AppendEvent(ctx, p.event); err != nil {
		return err
	}
	return nil
}

func (p PreparedPreferenceIssue) Changed() bool { return p.changed }
