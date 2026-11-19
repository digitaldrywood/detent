package executionapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type DependencyDeps struct {
	Repository      tenantstore.Repository
	Details         func() attachment.DetailStore
	AuthorizeAccess func(context.Context, accessstore.Reader, hubscope.NativeScope, bool) error
	WriteClaim      func(context.Context, hubscope.NativeScope, func(context.Context, tenantstore.Writer) error) error
	LoadPlan        func(context.Context, hubscope.NativeScope, string, string, any) (hubrecords.WorkItemRecordPlan, error)
	PlanWorkItem    func(context.Context, hubscope.NativeScope, hubrecords.WorkItemRecordPlan, any) (hubrecords.WorkItemRecordPlan, error)
	PrepareFields   func(context.Context, tracker.OrganizationID, hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemRecordFields, error)
}

type preparedRunnerDependency struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	history eventstore.Event
}

type PreparedRunnerDependencies struct {
	source    corestore.WorkItem
	Final     corestore.WorkItem
	Version   eventstore.CollaborationVersion
	endpoints []corestore.WorkItem
	changes   []preparedRunnerDependency
}

func NormalizeRunnerDependencyDisposition(data *tracker.NativeRunData, project projectstore.Metadata) {
	if data.Disposition == nil {
		return
	}
	disposition := *data.Disposition
	disposition.BlockerEvidence = slices.Clone(disposition.BlockerEvidence)
	normalize := func(reference string) string {
		prefix, number, found := strings.Cut(strings.TrimSpace(reference), "#")
		if found && prefix != "" && (strings.EqualFold(prefix, project.Settings.Repository) || strings.EqualFold(prefix, project.Settings.CheckoutRepository)) {
			return string(project.Core.ID) + "#" + number
		}
		return reference
	}
	for index := range disposition.BlockerEvidence {
		blocker := &disposition.BlockerEvidence[index]
		if blocker.Owner != workpad.BlockerOwnerOrchestrator || blocker.Unverifiable || blocker.Predicate == nil || blocker.Predicate.Type != workpad.PredicateIssueState {
			continue
		}
		blocker.Ref, blocker.Identifier = normalize(blocker.Ref), normalize(blocker.Identifier)
		if blocker.Predicate != nil {
			predicate := *blocker.Predicate
			predicate.Ref, predicate.Identifier = normalize(predicate.Ref), normalize(predicate.Identifier)
			blocker.Predicate = &predicate
		}
	}
	data.Disposition = &disposition
}

func (d DependencyDeps) PrepareRunnerDependencies(ctx context.Context, scope hubscope.NativeScope, source corestore.WorkItem, event tracker.NativeRunEvent) (*PreparedRunnerDependencies, bool, error) {
	if event.Type != "run.finished" || event.Data.Outcome != "succeeded" {
		return nil, false, nil
	}
	var signal *workpad.Signal
	if disposition := event.Data.Disposition; disposition != nil {
		signal = &workpad.Signal{Status: disposition.Status, ReasonCode: disposition.ReasonCode, Blockers: slices.Clone(disposition.BlockerEvidence)}
		if disposition.HumanAction {
			signal.HumanAction = "required"
		}
	}
	if signal == nil || signal.Status != workpad.StatusBlocked || signal.HumanAction != "" || signal.ReasonCode != "" || len(signal.Blockers) == 0 {
		return nil, false, nil
	}
	plan, err := d.LoadPlan(ctx, scope, string(source.NativeID), "", nil)
	if err != nil {
		return nil, false, err
	}
	if plan.Current.Revision != source.Revision || plan.Current.EventSequence != source.EventSequence {
		return nil, false, corestore.ErrSerializationFailure
	}
	plan.Issue.Profile = plan.Project.Core.Profile
	refs := workpad.IssueDependencyRefs(signal, string(scope.Project))
	if len(refs) == 0 {
		return nil, false, nil
	}
	if len(refs) > corestore.MaxPageSize {
		return nil, false, hubapi.NativePublicInvalid("Completion report contains too many dependencies")
	}
	result := &PreparedRunnerDependencies{source: source, Final: source, endpoints: []corestore.WorkItem{source}}
	var existing map[tracker.NativeWorkItemID]bool
	err = d.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := d.AuthorizeAccess(ctx, r, scope, false); err != nil {
			return err
		}
		ids := make([]tracker.NativeWorkItemID, 0, len(refs))
		for _, ref := range refs {
			number, err := strconv.ParseInt(strings.TrimPrefix(ref, string(scope.Project)+"#"), 10, 64)
			if err != nil {
				return tracker.ErrInvalidClaimRequest
			}
			target, err := r.WorkItemByNumber(ctx, scope.Project, number)
			if errors.Is(err, corestore.ErrNotFound) || errors.Is(err, sql.ErrNoRows) || err == nil && target.ID == source.ID {
				result.endpoints = nil
				return nil
			}
			if err != nil {
				return err
			}
			result.endpoints = append(result.endpoints, target)
			ids = append(ids, target.NativeID)
		}
		reader, ok := r.(interface {
			RunnerReportedDependencies(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, []tracker.NativeWorkItemID) (map[tracker.NativeWorkItemID]bool, error)
		})
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		existing, err = reader.RunnerReportedDependencies(ctx, scope.Project, source.NativeID, ids)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if len(result.endpoints) == 0 {
		return nil, false, nil
	}
	content := eventstore.NewContent(d.Details())
	for _, target := range result.endpoints[1:] {
		if existing[target.NativeID] {
			continue
		}
		plan.Current = result.Final
		plan.Issue.Dependencies = slices.Clone(plan.Issue.Dependencies)
		plan.Related = target
		plan.RelationshipOperate, plan.RelationshipType = "add", "dependency"
		plan.Relationship = &corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: source.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: target.NativeID, Type: "dependency"}
		plan, err = d.PlanWorkItem(ctx, scope, plan, tracker.DependencyMutation{ExpectedRevision: plan.Issue.Revision, RelatedWorkItemID: target.NativeID, Operation: "add"})
		if err != nil {
			return nil, false, err
		}
		change := preparedRunnerDependency{plan: plan}
		change.fields, err = d.PrepareFields(ctx, scope.Organization, plan)
		if err != nil {
			return nil, false, err
		}
		raw, err := json.Marshal(plan.Issue)
		if err != nil {
			return nil, false, err
		}
		change.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: source.NativeID, RecordID: string(source.NativeID), Revision: int64(plan.Issue.Revision)}, raw)
		if err != nil {
			return nil, false, err
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: source.NativeID, Type: "dependency.changed", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: plan.Issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, RelatedWorkItemID: target.NativeID, Operation: "add"}})
		if err != nil {
			return nil, false, err
		}
		change.history, err = content.PrepareEvent(ctx, scope.Organization, raw)
		if err != nil {
			return nil, false, err
		}
		result.changes = append(result.changes, change)
		result.Final.Revision = int64(plan.Issue.Revision)
		result.Final.EventSequence++
		result.Version = change.version
	}
	if len(result.changes) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

func (d DependencyDeps) WriteRunnerDependenciesCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, dependencies *PreparedRunnerDependencies, write func(context.Context, tenantstore.Writer) error) error {
	return d.WriteClaim(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := d.AuthorizeAccess(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockRelationshipGraph(ctx); err != nil {
			return err
		}
		_, receiptErr := w.Receipt(ctx, command.ActorID, command.Operation, command.Key)
		if receiptErr == nil {
			return write(ctx, w)
		}
		if !errors.Is(receiptErr, corestore.ErrNotFound) && !errors.Is(receiptErr, sql.ErrNoRows) {
			return receiptErr
		}
		if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || !hubrecords.SameWorkItemProjectPlan(dependencies.changes[0].plan.Project, project) {
			return corestore.ErrSerializationFailure
		}
		endpoints := slices.Clone(dependencies.endpoints)
		slices.SortFunc(endpoints, func(a, b corestore.WorkItem) int { return strings.Compare(string(a.NativeID), string(b.NativeID)) })
		for _, endpoint := range endpoints {
			current, err := w.LockWorkItem(ctx, scope.Project, endpoint.NativeID)
			if err != nil {
				return err
			}
			if current.ID != endpoint.ID || current.Revision != endpoint.Revision || current.ID == dependencies.source.ID && current.EventSequence != dependencies.source.EventSequence || !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(current.ActorPrincipalID)}) {
				return corestore.ErrSerializationFailure
			}
		}
		return write(ctx, w)
	})
}

func ApplyRunnerDependencies(ctx context.Context, w tenantstore.Writer, dependencies *PreparedRunnerDependencies) error {
	for _, change := range dependencies.changes {
		if err := w.SetRelationship(ctx, *change.plan.Relationship, "add", change.plan.Issue.UpdatedAt); err != nil {
			return err
		}
		if err := hubrecords.WriteWorkItemRecord(ctx, w, change.plan, change.fields); err != nil {
			return fmt.Errorf("write completion dependency item: %w", err)
		}
		if _, err := w.CreateCollaborationVersion(ctx, change.version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, change.history); err != nil {
			return err
		}
	}
	return nil
}
