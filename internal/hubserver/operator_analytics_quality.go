package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) readAnalyticsLandingPopulationTenant(ctx context.Context, scope hubscope.NativeScope, w operatortool.AnalyticsWindow, name string, reopened []tracker.NativeWorkItemID, escaped []string) (analyticsLandingPopulation, error) {
	out := analyticsLandingPopulation{Items: []nativeAnalyticsLanding{}}
	after, cursor, key, err := s.analyticsFactSeek(ctx, scope, w, name)
	if err != nil {
		return out, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, hubapi.NativeWorkItemStorageUnavailable()
	}
	var page eventstore.AnalyticsLandingPage
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(eventstore.AnalyticsLandingReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsLandingPage(ctx, eventstore.AnalyticsLandingQuery{ProjectID: scope.Project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, AllVersions: name == "quality_landings", After: after.Landing, ReopenedItems: reopened, EscapedVersions: escaped})
		return err
	})
	if err != nil {
		return out, err
	}
	out.Partial, out.HistoryUnknown = page.Partial, page.HistoryUnknown
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	for _, row := range page.Items {
		if row.OrganizationID != scope.Organization || row.ProjectID != scope.Project {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		value, err := content.VersionLanding(ctx, row)
		if err != nil {
			return out, err
		}
		if name == "landings" {
			value.Quality = nil
		}
		out.Items = append(out.Items, nativeAnalyticsLanding{ChangeID: row.ChangeID, WorkItemID: row.WorkItemID, Landing: value})
	}
	if page.Next.ChangeID != "" {
		if len(key) == 0 {
			out.Partial = true
			out.ContinuationUnavailable = true
		} else {
			token, err := s.encodeAnalyticsFactCursor(cursor, key, analyticsFactCursor{Landing: page.Next}, name)
			if err != nil {
				return out, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.Next[name] = token
			}
		}
	}
	return out, nil
}

func (s *Service) readAnalyticsQualityInputsTenant(ctx context.Context, scope hubscope.NativeScope, w operatortool.AnalyticsWindow) (analyticsQualityInputs, error) {
	out := analyticsQualityInputs{Landings: []nativeAnalyticsLanding{}, Events: []qualityHistoryEvent{}, Discussions: []qualityDiscussion{}}
	ctx, _, err := analyticsPagingContext(ctx, "")
	if err != nil {
		return out, err
	}
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, hubapi.NativeWorkItemStorageUnavailable()
	}
	after, eventCursor, eventKey, err := s.analyticsFactSeek(ctx, scope, w, "quality_events")
	if err != nil {
		return out, err
	}
	defer clear(eventKey)
	evidenceAfter, evidenceCursor, evidenceKey, err := s.analyticsFactSeek(ctx, scope, w, "quality_evidence")
	if err != nil {
		return out, err
	}
	defer clear(evidenceKey)
	var workflow eventstore.AnalyticsWorkflowPage
	var evidence eventstore.AnalyticsEvidencePage
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		workflowOwner, ok := r.(eventstore.AnalyticsWorkflowReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		evidenceOwner, ok := r.(eventstore.AnalyticsEvidenceReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		workflow, err = workflowOwner.ReadAnalyticsWorkflowPage(ctx, eventstore.AnalyticsWorkflowQuery{ProjectID: scope.Project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after.Workflow})
		if err != nil {
			return err
		}
		evidence, err = evidenceOwner.ReadAnalyticsEvidencePage(ctx, eventstore.AnalyticsEvidenceQuery{ProjectID: scope.Project, Principal: principal, From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: evidenceAfter.Evidence})
		return err
	})
	if err != nil {
		return out, err
	}
	out.Partial = workflow.Partial || evidence.Partial
	content := eventstore.NewContent(s.runDetailStoreFor(ctx))
	reopened := []tracker.NativeWorkItemID{}
	seenReopened := map[tracker.NativeWorkItemID]bool{}
	escaped := []string{}
	seenEscaped := map[string]bool{}
	for _, row := range workflow.Items {
		if row.Event.OrganizationID != scope.Organization || row.Event.ProjectID != scope.Project {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.Event.WorkItemID) {
			continue
		}
		value, err := content.Event(ctx, row.Event)
		if err != nil {
			return out, err
		}
		out.Events = append(out.Events, qualityHistoryEvent{ID: value.ID, Item: string(value.AggregateID), Kind: value.Type, ToTerminal: row.ToTerminal, At: value.RecordedAt, Data: value.Data})
		if strings.EqualFold(value.Data.FromState, "Done") && !seenReopened[value.AggregateID] {
			seenReopened[value.AggregateID] = true
			reopened = append(reopened, value.AggregateID)
		}
	}
	for _, row := range evidence.Items {
		if row.Evidence.OrganizationID != scope.Organization {
			return out, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		raw, err := content.Evidence(ctx, row.Evidence)
		if err != nil {
			return out, err
		}
		if row.Evidence.Kind == "review" {
			review, err := eventstore.DecodeChangeReview(row.Evidence, raw)
			if err != nil {
				return out, err
			}
			out.Events = append(out.Events, qualityHistoryEvent{ID: review.ID, Item: string(row.WorkItemID), Kind: "changes_requested", VersionID: review.VersionID, At: review.CreatedAt})
		} else {
			var value tracker.ChangeDiscussion
			version := ""
			if row.Evidence.VersionID != nil {
				version = *row.Evidence.VersionID
			}
			if json.Unmarshal(raw, &value) != nil || value.ID == "" || value.VersionID != version || value.Actor.Kind != row.Evidence.ActorKind || value.Actor.PrincipalID != row.Evidence.ActorPrincipalID || !value.CreatedAt.UTC().Truncate(time.Microsecond).Equal(row.Evidence.CreatedAt.UTC().Truncate(time.Microsecond)) || value.Escape == nil || row.Escape == nil || value.Escape.OccurrenceID != row.Escape.OccurrenceID {
				return out, artifact.ErrIntegrity
			}
			value.Escape.Kind, value.Escape.Cause, value.Escape.ObservedAt, value.Escape.InstanceID, value.Escape.HumanOverride = row.Escape.Kind, row.Escape.Cause, row.Escape.ObservedAt, row.Escape.InstanceID, row.Escape.HumanOverride
			out.Discussions = append(out.Discussions, qualityDiscussion{ChangeID: row.Evidence.ChangeID, ChangeDiscussion: value})
			identity := row.Evidence.ChangeID + "/" + value.VersionID
			if !seenEscaped[identity] {
				seenEscaped[identity] = true
				escaped = append(escaped, identity)
			}
		}
	}
	paging := analyticsPagingFromContext(ctx)
	if workflow.Next.ID != "" {
		if len(eventKey) == 0 {
			out.Partial = true
			out.Unavailable = append(out.Unavailable, "quality_events_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(eventCursor, eventKey, analyticsFactCursor{Workflow: workflow.Next}, "quality_events")
			if err != nil {
				return out, err
			}
			paging.Next["quality_events"] = token
		}
	}
	if evidence.Next.ChangeID != "" {
		if len(evidenceKey) == 0 {
			out.Partial = true
			out.Unavailable = append(out.Unavailable, "quality_evidence_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsFactCursor(evidenceCursor, evidenceKey, analyticsFactCursor{Evidence: evidence.Next}, "quality_evidence")
			if err != nil {
				return out, err
			}
			paging.Next["quality_evidence"] = token
		}
	}
	landings, err := s.readAnalyticsLandingPopulationTenant(ctx, scope, w, "quality_landings", reopened, escaped)
	if err != nil {
		return out, err
	}
	out.Landings = landings.Items
	out.Partial = out.Partial || landings.Partial
	if landings.HistoryUnknown {
		out.Unavailable = append(out.Unavailable, "quality_landing_history_unavailable")
	}
	if landings.ContinuationUnavailable {
		out.Unavailable = append(out.Unavailable, "quality_landings_continuation_requires_secret_keys")
	}
	worked, partial, unavailable, err := s.readAnalyticsWorkedPopulationTenant(ctx, scope, w)
	if err != nil {
		return out, err
	}
	out.Events = append(out.Events, worked...)
	out.Partial = out.Partial || partial
	out.Unavailable = append(out.Unavailable, unavailable...)
	return out, nil
}

func (s *Service) readAnalyticsWorkedPopulationTenant(ctx context.Context, scope hubscope.NativeScope, w operatortool.AnalyticsWindow) ([]qualityHistoryEvent, bool, []string, error) {
	out := []qualityHistoryEvent{}
	unavailable := []string{}
	after, cursor, key, err := s.analyticsAttemptCursor(ctx, scope, w, "", "quality_worked")
	if err != nil {
		return out, false, nil, err
	}
	defer clear(key)
	repository, ok := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	if !ok {
		return out, false, nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	var page reportstore.AnalyticsAttemptPage
	unknownEvents := map[string]eventstore.Event{}
	err = repository.ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		principal, err := s.analyticsSnapshotPrincipal(ctx, r, scope)
		if err != nil {
			return err
		}
		owner, ok := r.(reportstore.AnalyticsAttemptReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		page, err = owner.ReadAnalyticsAttemptPage(ctx, reportstore.AnalyticsAttemptQuery{ProjectID: scope.Project, Principal: principal, RunnerIDs: analyticsCohortRunners(ctx), From: w.From, To: w.To, Limit: maxAnalyticsPopulation, After: after, StartedInWindow: true, OldestFirst: true})
		if err != nil {
			return err
		}
		unknown := []string{}
		for _, row := range page.Items {
			if analyticsCohortAllows(ctx, row.WorkItemID) && (row.Role == nil || *row.Role == "") {
				unknown = append(unknown, row.AttemptID)
			}
		}
		if len(unknown) == 0 {
			return nil
		}
		batch, ok := r.(eventstore.RunEventBatchReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		events, err := batch.LatestRunEvents(ctx, scope.Project, unknown)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.OrganizationID != scope.Organization || event.ProjectID != scope.Project || event.AttemptID == "" {
				return artifact.ErrIntegrity
			}
			if _, duplicate := unknownEvents[event.AttemptID]; duplicate {
				return artifact.ErrIntegrity
			}
			unknownEvents[event.AttemptID] = event
		}
		return nil
	})
	if err != nil {
		return out, false, nil, err
	}
	partial := page.Partial
	for _, row := range page.Items {
		if row.ProjectID != scope.Project {
			return out, false, nil, artifact.ErrIntegrity
		}
		if !analyticsCohortAllows(ctx, row.WorkItemID) {
			continue
		}
		role := hubapi.StringValue(row.Role)
		if role == "" {
			event, ok := unknownEvents[row.AttemptID]
			if !ok {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
			if event.WorkItemID != row.WorkItemID {
				return out, false, nil, artifact.ErrIntegrity
			}
			value, _, err := eventstore.NewContent(s.runDetailStoreFor(ctx)).AnalyticsEvent(ctx, event)
			if errors.Is(err, artifact.ErrMissing) {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
			if err != nil {
				return out, false, nil, err
			}
			if value.Data.Run == nil || value.Data.Run.AttemptID != row.AttemptID {
				return out, false, nil, artifact.ErrIntegrity
			}
			if value.Data.Run.Runtime != nil {
				role = value.Data.Run.Runtime.Identity.Role
			}
			if role == "" && value.Data.Run.Identity != nil {
				role = value.Data.Run.Identity.Role
			}
			if role == "" {
				partial = true
				unavailable = append(unavailable, "quality_worked_role_unavailable")
				continue
			}
		}
		if role == "code" || role == "rework" {
			out = append(out, qualityHistoryEvent{Item: string(row.WorkItemID), Kind: "worked", At: row.StartedAt})
		}
	}
	if page.Next.ID != "" {
		if len(key) == 0 {
			partial = true
			unavailable = append(unavailable, "quality_worked_continuation_requires_secret_keys")
		} else {
			token, err := s.encodeAnalyticsAttemptCursor(cursor, key, page.Next, "quality_worked")
			if err != nil {
				return out, false, nil, err
			}
			if paging := analyticsPagingFromContext(ctx); paging != nil {
				paging.Next["quality_worked"] = token
			}
		}
	}
	return out, partial, slices.Compact(unavailable), nil
}

func (s *Service) readAnalyticsQualityTenant(ctx context.Context, scope hubscope.NativeScope, r operatortool.AnalyticsRequest, w operatortool.AnalyticsWindow) (analyticsQuality, error) {
	inputs, err := s.readAnalyticsQualityInputsTenant(ctx, scope, w)
	if err != nil {
		return analyticsQuality{}, err
	}
	return projectAnalyticsQualityInputs(inputs, r, w)
}
