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
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

type preparedRunnerDependency struct {
	plan    workItemRecordPlan
	fields  workItemRecordFields
	version eventstore.CollaborationVersion
	history eventstore.Event
	search  searchstore.Document
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

func (s *Service) prepareRunnerDependencies(ctx context.Context, scope nativeScope, source corestore.WorkItem, event tracker.NativeRunEvent) (*preparedRunnerDependencies, bool, error) {
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
	if plan.current.Revision != source.Revision || plan.current.EventSequence != source.EventSequence {
		return nil, false, corestore.ErrSerializationFailure
	}
	plan.issue.Profile = plan.project.Core.Profile
	refs := workpad.IssueDependencyRefs(signal, string(scope.project))
	if len(refs) == 0 {
		return nil, false, nil
	}
	if len(refs) > corestore.MaxPageSize {
		return nil, false, nativePublicInvalid("Completion report contains too many dependencies")
	}
	result := &preparedRunnerDependencies{source: source, final: source, endpoints: []corestore.WorkItem{source}}
	var existing map[tracker.NativeWorkItemID]bool
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		ids := make([]tracker.NativeWorkItemID, 0, len(refs))
		for _, ref := range refs {
			number, err := strconv.ParseInt(strings.TrimPrefix(ref, string(scope.project)+"#"), 10, 64)
			if err != nil {
				return tracker.ErrInvalidClaimRequest
			}
			target, err := r.WorkItemByNumber(ctx, scope.project, number)
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
			return nativeRunEventStorageUnavailable()
		}
		var err error
		existing, err = reader.RunnerReportedDependencies(ctx, scope.project, source.NativeID, ids)
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
		plan.current = result.final
		plan.issue.Dependencies = slices.Clone(plan.issue.Dependencies)
		plan.related = target
		plan.relationshipOperate, plan.relationshipType = "add", "dependency"
		plan.relationship = &corestore.Relationship{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: source.NativeID, RelatedProjectID: scope.project, RelatedWorkItemID: target.NativeID, Type: "dependency"}
		plan, err = s.planWorkItemRecord(ctx, scope, plan, tracker.DependencyMutation{ExpectedRevision: plan.issue.Revision, RelatedWorkItemID: target.NativeID, Operation: "add"})
		if err != nil {
			return nil, false, err
		}
		change := preparedRunnerDependency{plan: plan}
		change.fields, err = s.prepareWorkItemFields(ctx, scope.organization, plan)
		if err != nil {
			return nil, false, err
		}
		raw, err := json.Marshal(plan.issue)
		if err != nil {
			return nil, false, err
		}
		change.version, err = content.PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: source.NativeID, RecordID: string(source.NativeID), Revision: int64(plan.issue.Revision)}, raw)
		if err != nil {
			return nil, false, err
		}
		change.search, err = searchstore.NewContent(s.runDetailStore()).Prepare(ctx, searchstore.Document{OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: source.NativeID, ID: "issue:" + string(source.NativeID), SourceID: string(source.NativeID), Kind: "issue", Scope: "issue", Revision: int64(plan.issue.Revision), UpdatedAt: plan.issue.UpdatedAt}, searchstore.Text{Title: plan.issue.Title, Body: plan.issue.Body, Labels: plan.issue.Labels})
		if err != nil {
			return nil, false, err
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("evt"), OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: source.NativeID, Type: "dependency.changed", SchemaVersion: 1, Actor: scope.actor(), RecordedAt: plan.issue.UpdatedAt, Data: tracker.CollaborationData{Revision: plan.issue.Revision, RelatedWorkItemID: target.NativeID, Operation: "add"}})
		if err != nil {
			return nil, false, err
		}
		change.history, err = content.PrepareEvent(ctx, scope.organization, raw)
		if err != nil {
			return nil, false, err
		}
		result.changes = append(result.changes, change)
		result.final.Revision = int64(plan.issue.Revision)
		result.final.EventSequence++
		result.version = change.version
	}
	if len(result.changes) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

func (s *Service) writeRunnerDependenciesCommand(ctx context.Context, scope nativeScope, command eventstore.RunCommand, dependencies *preparedRunnerDependencies, write func(context.Context, tenantstore.Writer) error) error {
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
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || !sameWorkItemProjectPlan(dependencies.changes[0].plan.project, project) {
			return corestore.ErrSerializationFailure
		}
		endpoints := slices.Clone(dependencies.endpoints)
		slices.SortFunc(endpoints, func(a, b corestore.WorkItem) int { return strings.Compare(string(a.NativeID), string(b.NativeID)) })
		for _, endpoint := range endpoints {
			current, err := w.LockWorkItem(ctx, scope.project, endpoint.NativeID)
			if err != nil {
				return err
			}
			if current.ID != endpoint.ID || current.Revision != endpoint.Revision || current.ID == dependencies.source.ID && current.EventSequence != dependencies.source.EventSequence || !scope.reporterOwns(tracker.Actor{PrincipalID: stringValue(current.ActorPrincipalID)}) {
				return corestore.ErrSerializationFailure
			}
		}
		return write(ctx, w)
	})
}

func applyRunnerDependencies(ctx context.Context, w tenantstore.Writer, dependencies *preparedRunnerDependencies) error {
	for _, change := range dependencies.changes {
		if err := w.SetRelationship(ctx, *change.plan.relationship, "add", change.plan.issue.UpdatedAt); err != nil {
			return err
		}
		if err := writeWorkItemRecord(ctx, w, change.plan, change.fields); err != nil {
			return fmt.Errorf("write completion dependency item: %w", err)
		}
		if _, err := w.CreateCollaborationVersion(ctx, change.version); err != nil {
			return err
		}
		if _, err := w.AppendEvent(ctx, change.history); err != nil {
			return err
		}
		if err := w.PutSearch(ctx, change.search, change.plan.current.Revision); err != nil {
			return fmt.Errorf("write completion dependency search: %w", err)
		}
	}
	return nil
}
