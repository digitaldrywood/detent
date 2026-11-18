package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const runnerAdmissionDecisionLimit = 16

type preparedRunnerAdmissionDecisions struct {
	project   tracker.ProjectID
	runnerID  string
	decisions []preparedRunnerAdmissionDecision
}

type preparedRunnerAdmissionDecision struct {
	key eventstore.RunnerAdmissionDecision
	row eventstore.Event
}

func runnerAdmissionDecisionCandidates(scope hubscope.NativeScope, view *runnerauth.ProjectConfiguration, now time.Time) (map[tracker.NativeWorkItemID]runnerauth.DiagnosticAdmission, []eventstore.RunnerAdmissionDecision, error) {
	if view == nil || view.Diagnostics == nil || scope.Project == "" {
		return nil, nil, nil
	}
	latest := make(map[tracker.NativeWorkItemID]runnerauth.DiagnosticAdmission)
	for _, admission := range view.Diagnostics.Admissions {
		if admission.Result != "skipped" || admission.WorkItemRevision < 1 || strings.TrimSpace(admission.IssueID) == "" || admission.ObservedAt.IsZero() || admission.ObservedAt.After(now) {
			continue
		}
		item := tracker.NativeWorkItemID(admission.IssueID)
		if previous, ok := latest[item]; ok && previous.ObservedAt.After(admission.ObservedAt) {
			continue
		}
		latest[item] = admission
	}
	keys := make([]eventstore.RunnerAdmissionDecision, 0, len(latest))
	for item, admission := range latest {
		observed := admission.ObservedAt.UTC().Truncate(time.Microsecond)
		raw, err := json.Marshal(struct {
			Organization tracker.OrganizationID
			Project      tracker.ProjectID
			Item         tracker.NativeWorkItemID
			Runner       string
			Revision     int64
			Predicate    string
			ObservedAt   time.Time
		}{scope.Organization, scope.Project, item, scope.Credential.Runner.RunnerID, admission.WorkItemRevision, admission.Predicate, observed})
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, eventstore.RunnerAdmissionDecision{WorkItemID: item, Revision: admission.WorkItemRevision, ObservedAt: observed, EventID: "evt_admission_" + policy.Digest(raw)})
	}
	slices.SortFunc(keys, func(left, right eventstore.RunnerAdmissionDecision) int {
		if order := right.ObservedAt.Compare(left.ObservedAt); order != 0 {
			return order
		}
		return strings.Compare(string(left.WorkItemID), string(right.WorkItemID))
	})
	return latest, keys, nil
}

func (s *Service) prepareRunnerAdmissionDecisions(ctx context.Context, credential hubapi.APICredential, view *runnerauth.ProjectConfiguration) (preparedRunnerAdmissionDecisions, error) {
	var prepared preparedRunnerAdmissionDecisions
	if view == nil {
		return prepared, nil
	}
	scope := hubscope.NativeScope{Organization: credential.Runner.OrganizationID, Project: tracker.ProjectID(view.ProjectID), Credential: credential}
	admissions, keys, err := runnerAdmissionDecisionCandidates(scope, view, s.config.Clock().UTC())
	if err != nil || len(keys) == 0 {
		return prepared, err
	}
	var pending []eventstore.RunnerAdmissionDecision
	if err := s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		reader, ok := r.(eventstore.RunnerAdmissionDecisionReader)
		if !ok {
			return hubapi.NativeRunEventStorageUnavailable()
		}
		var err error
		pending, err = reader.PendingRunnerAdmissionDecisions(ctx, scope.Project, keys)
		return err
	}); err != nil {
		return prepared, err
	}
	if len(pending) > runnerAdmissionDecisionLimit {
		pending = pending[:runnerAdmissionDecisionLimit]
	}
	prepared = preparedRunnerAdmissionDecisions{project: scope.Project, runnerID: credential.Runner.RunnerID}
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, key := range pending {
		admission := admissions[key.WorkItemID]
		decision := tracker.CollaborationEvent{ID: key.EventID, OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateType: "issue", AggregateID: key.WorkItemID, Type: "scheduler.decision", SchemaVersion: 1, Actor: scope.Actor(), RecordedAt: key.ObservedAt, Data: tracker.CollaborationData{Revision: tracker.Revision(key.Revision), Decision: &tracker.NativeSchedulerDecision{RunnerID: credential.Runner.RunnerID, Source: "native_runner_admission", Outcome: admission.Result, Reason: admission.Predicate, At: key.ObservedAt, WorkItemRevision: tracker.Revision(key.Revision)}}}
		raw, err := json.Marshal(decision)
		if err != nil {
			return preparedRunnerAdmissionDecisions{}, err
		}
		row, err := content.PrepareEvent(ctx, scope.Organization, raw)
		if err != nil {
			return preparedRunnerAdmissionDecisions{}, err
		}
		prepared.decisions = append(prepared.decisions, preparedRunnerAdmissionDecision{key: key, row: row})
	}
	return prepared, nil
}

func appendRunnerAdmissionDecisions(ctx context.Context, w tenantstore.Writer, identity runnerstore.Identity, prepared preparedRunnerAdmissionDecisions, receivedAt time.Time) error {
	if len(prepared.decisions) == 0 {
		return nil
	}
	if prepared.runnerID != identity.RunnerID {
		return hubscope.RunnerUnauthorized()
	}
	allowed, err := w.RunnerProjectAuthorized(ctx, identity.RunnerID, prepared.project, receivedAt)
	if err != nil {
		return err
	}
	if !allowed {
		return hubapi.NativeNotFound()
	}
	owner, ok := w.(eventstore.RunnerAdmissionDecisionWriter)
	if !ok {
		return hubapi.NativeRunEventStorageUnavailable()
	}
	keys := make([]eventstore.RunnerAdmissionDecision, 0, len(prepared.decisions))
	items := make([]tracker.NativeWorkItemID, 0, len(prepared.decisions))
	for _, decision := range prepared.decisions {
		if !decision.key.ObservedAt.After(receivedAt) {
			keys = append(keys, decision.key)
			items = append(items, decision.key.WorkItemID)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if err := owner.LockRunnerAdmissionItems(ctx, prepared.project, items); err != nil {
		return err
	}
	pending, err := owner.PendingRunnerAdmissionDecisions(ctx, prepared.project, keys)
	if err != nil {
		return err
	}
	current := make(map[string]bool, len(pending))
	for _, key := range pending {
		current[key.EventID] = true
	}
	for _, decision := range prepared.decisions {
		if !current[decision.key.EventID] {
			continue
		}
		if _, err := w.AppendEvent(ctx, decision.row); err != nil {
			return err
		}
	}
	return nil
}
