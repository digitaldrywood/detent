package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthIssuePublication struct {
	work     healthstore.IssueWork
	report   *healthstore.IssueReport
	resolve  tracker.NativeWorkItemID
	cutoff   time.Time
	openedAt time.Time
	linked   tracker.NativeWorkItemID
}

func authorizeHealthPublication(ctx context.Context, reader accessstore.Reader, scope nativeScope, write bool) error {
	publication := scope.healthPublication
	owner, ok := reader.(healthstore.IssueReader)
	if !ok || publication == nil || scope.sourceActor == nil || *scope.sourceActor != *nativeIntegrationActor("health_detector") || scope.organization != publication.work.OrganizationID || scope.project != publication.work.ProjectID {
		return accessstore.ErrDenied
	}
	if err := owner.AuthorizeHealthIssue(ctx, publication.work.FindingID, publication.work.SourceRevision, scope.project, write); err != nil {
		return err
	}
	if publication.resolve != "" {
		allowed, err := owner.CanResolveHealthIssue(ctx, scope.project, publication.resolve, publication.cutoff)
		if err != nil {
			return err
		}
		if !allowed {
			return accessstore.ErrDenied
		}
	}
	return nil
}

func recordHealthPublication(ctx context.Context, writer tenantstore.Writer, scope nativeScope, plan workItemRecordPlan) error {
	if scope.healthPublication == nil {
		return nil
	}
	if err := authorizeHealthPublication(ctx, writer, scope, true); err != nil {
		return err
	}
	if scope.healthPublication.resolve != "" {
		active, err := writer.WorkItemHasActiveLease(ctx, scope.project, scope.healthPublication.resolve, plan.issue.UpdatedAt)
		if err != nil {
			return err
		}
		if active {
			return tracker.ErrLeaseConflict
		}
	}
	if scope.healthPublication.report == nil {
		return nil
	}
	if plan.existing || plan.comment.ID != "" {
		current, err := writer.LockWorkItem(ctx, scope.project, plan.issue.WorkItemID)
		if err != nil {
			return err
		}
		if current.Revision != int64(plan.issue.Revision) {
			return corestore.ErrSerializationFailure
		}
	}
	owner, ok := writer.(healthstore.IssueWriter)
	if !ok {
		return workItemStorageUnavailable()
	}
	row := *scope.healthPublication.report
	row.WorkItemID = plan.issue.WorkItemID
	if plan.comment.ID != "" {
		row.WorkItemID = plan.comment.WorkItemID
	}
	return owner.RecordHealthIssueReport(ctx, row)
}

func (s *Service) filterHealthPublicationDuplicates(ctx context.Context, scope nativeScope, search *workItemDuplicateSearch) error {
	if scope.healthPublication.report != nil {
		if err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
			latest, err := r.(healthstore.IssueReader).LatestHealthIssueReport(ctx, scope.project, scope.healthPublication.report.IntakeFingerprint)
			if errors.Is(err, corestore.ErrNotFound) {
				scope.healthPublication.linked = ""
				return nil
			}
			if err == nil {
				scope.healthPublication.linked = latest.WorkItemID
			}
			return err
		}); err != nil {
			return err
		}
	}
	if linked := scope.healthPublication.linked; linked != "" {
		issue, err := s.readWorkItem(ctx, scope, string(linked))
		if err != nil {
			return err
		}
		if !issue.Archived {
			search.matches = []possibleDuplicate{{WorkItemID: issue.WorkItemID, Number: issue.Number, Title: issue.Title, State: issue.State, revision: issue.Revision, terminal: issue.Terminal}}
		}
	}
	retained := search.matches[:0]
	for _, candidate := range search.matches {
		issue, err := s.readWorkItem(ctx, scope, string(candidate.WorkItemID))
		if err != nil {
			return err
		}
		renew := issue.Terminal && issue.State == "Done" && issue.Actor == scope.actor() && issue.Provenance == nil && healthDetectorIssueBody(issue.Body)
		if issue.Terminal && issue.State == "Cancelled" && issue.Actor == scope.actor() && issue.Provenance == nil && healthDetectorIssueBody(issue.Body) {
			err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				renew, err = r.(healthstore.IssueReader).HealthIssueAutoCancelledBefore(ctx, scope.project, issue.WorkItemID, scope.healthPublication.openedAt)
				return err
			})
			if err != nil {
				return err
			}
		}
		if !renew {
			retained = append(retained, candidate)
		}
	}
	search.matches = retained
	return nil
}

func healthDetectorIssueBody(body string) bool {
	origin, ok := issueorigin.Parse(body)
	return ok && origin.Kind == "audit" && origin.Instance == "health_detector" && origin.Source != ""
}

func (s *Service) readHealthIssue(ctx context.Context, scope nativeScope, id string) (tracker.NativeIssue, error) {
	row, version, err := s.readWorkItemRecord(ctx, scope, id)
	if err != nil {
		return tracker.NativeIssue{}, fmt.Errorf("read health issue metadata: %w", err)
	}
	issue, err := s.hydrateWorkItemRecord(ctx, row, version)
	if err != nil {
		return tracker.NativeIssue{}, fmt.Errorf("read health issue content: %w", err)
	}
	return issue, nil
}

type healthIssueCancellation struct {
	tracker.Transition
}

func planHealthIssueCancellation(scope nativeScope, plan workItemRecordPlan, request healthIssueCancellation, now time.Time) (workItemRecordPlan, error) {
	if scope.healthPublication == nil || scope.healthPublication.resolve != plan.issue.WorkItemID || request.State != "Cancelled" || request.Reason != "worker_progress" || plan.issue.State != "Backlog" || plan.issue.Actor != *nativeIntegrationActor("health_detector") || plan.issue.Provenance != nil || !healthDetectorIssueBody(plan.issue.Body) {
		return plan, accessstore.ErrDenied
	}
	if err := requireNativeEdit(plan.issue, request.ExpectedRevision); err != nil {
		return plan, err
	}
	if !slices.ContainsFunc(plan.project.States, func(state tracker.NativeState) bool {
		return state.Name == "Cancelled" && state.Terminal && !state.OperatorOnly
	}) {
		return plan, workItemStorageUnavailable()
	}
	if err := validateNativeWorkflowTransition(plan.project.States, plan.issue.State, "Cancelled", apiScopeWorker); err != nil {
		return plan, err
	}
	plan.issue.State, plan.issue.Terminal = "Cancelled", true
	plan.issue.ClosedAt = &now
	plan.issue.UpdatedAt, plan.issue.LastActivityAt = now, now
	plan.issue.Revision++
	plan.eventType = "workflow.transitioned"
	return plan, nil
}

func (s *Service) cancelHealthIssue(ctx context.Context, scope nativeScope, item string, request tracker.Transition) error {
	scope.action, scope.actionItem, scope.actionTarget = operatortool.MoveItem, item, "Cancelled"
	_, err := s.executeWorkItemRecordMutation(ctx, scope, nativeCommandOptions{OperationID: nativeOperation(scope, http.MethodPost, "/work-items/"+item+"/workflow"), Item: item, Feature: "collaboration"}, request.Mutation, healthIssueCancellation{request}, "")
	return err
}
