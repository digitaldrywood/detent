package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

type preparedRunnerDependency struct {
	plan    hubrecords.WorkItemRecordPlan
	fields  hubrecords.WorkItemRecordFields
	version eventstore.CollaborationVersion
	history eventstore.Event
}

type preparedRunnerDependencies struct {
	source    corestore.WorkItem
	final     corestore.WorkItem
	version   eventstore.CollaborationVersion
	endpoints []corestore.WorkItem
	changes   []preparedRunnerDependency
}

func normalizeRunnerDependencyDisposition(data *tracker.NativeRunData, project projectstore.Record) {
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

func (s *Service) prepareRunnerDependencies(ctx context.Context, scope hubscope.NativeScope, source corestore.WorkItem, event tracker.NativeRunEvent) (*preparedRunnerDependencies, bool, error) {
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
	plan, err := s.loadWorkItemPlan(ctx, scope, string(source.NativeID), "", nil)
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
	result := &preparedRunnerDependencies{source: source, final: source, endpoints: []corestore.WorkItem{source}}
	var existing map[tracker.NativeWorkItemID]bool
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
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
	content := eventstore.NewContent(s.runDetailStore())
	for _, target := range result.endpoints[1:] {
		if existing[target.NativeID] {
			continue
		}
		plan.Current = result.final
		plan.Issue.Dependencies = slices.Clone(plan.Issue.Dependencies)
		plan.Related = target
		plan.RelationshipOperate, plan.RelationshipType = "add", "dependency"
		plan.Relationship = &corestore.Relationship{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: source.NativeID, RelatedProjectID: scope.Project, RelatedWorkItemID: target.NativeID, Type: "dependency"}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, tracker.DependencyMutation{ExpectedRevision: plan.Issue.Revision, RelatedWorkItemID: target.NativeID, Operation: "add"})
		if err != nil {
			return nil, false, err
		}
		change := preparedRunnerDependency{plan: plan}
		change.fields, err = s.prepareWorkItemFields(ctx, scope.Organization, plan)
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
		result.final.Revision = int64(plan.Issue.Revision)
		result.final.EventSequence++
		result.version = change.version
	}
	if len(result.changes) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

func (s *Service) writeRunnerDependenciesCommand(ctx context.Context, scope hubscope.NativeScope, command eventstore.RunCommand, dependencies *preparedRunnerDependencies, write func(context.Context, tenantstore.Writer) error) error {
	return s.writeNativeClaimCommand(ctx, scope, func(ctx context.Context, w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
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

func applyRunnerDependencies(ctx context.Context, w tenantstore.Writer, dependencies *preparedRunnerDependencies) error {
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
