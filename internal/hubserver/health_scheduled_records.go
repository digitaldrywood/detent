package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) recordHealthScheduledValidation(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	report := request.ScheduledValidation
	if report == nil || !report.Valid() || request.ScheduledFailure != nil || request.ChallengeToken != "" || len(request.NotDuplicateOf) != 0 || request.Type != "" || request.ReportContext != nil || request.Title != "" || request.Body != "" || request.State != "" || request.Priority != nil || len(request.Labels) != 0 || len(request.Assignees) != 0 || request.Provenance != nil || request.GitHubIssueURL != "" {
		return nil, nativePublicInvalid("Invalid scheduled validation report")
	}
	if scope.credential.Scope == apiScopeWorker || scope.credential.HostedRole == "reporter" {
		return nil, &nativeError{Code: "insufficient_scope", Message: "Scheduled reporting requires operator authority", status: http.StatusForbidden, publicMessage: true}
	}
	scope.action = operatortool.FileIssue
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	var retained healthstore.ScheduledReport
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		project, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if project.Core.DeletedAt != nil || project.Core.Profile != "native" {
			return nativeNotFound()
		}
		repository := project.Settings.Repository
		if repository == "" {
			repository = project.Settings.CheckoutRepository
		}
		if !strings.EqualFold(repository, report.Repository) {
			return nativePublicInvalid("Scheduled validation repository does not match the project")
		}
		owner, ok := w.(healthstore.ScheduledWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		retained, err = owner.RecordHealthScheduledReport(ctx, healthstore.ScheduledReport{ScheduledValidationReport: *report, OrganizationID: scope.organization, ProjectID: scope.project, RecordedAt: s.config.now().UTC()})
		return err
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nil, nativeConflict(0)
	}
	if err != nil {
		return nil, projectRecordError(err)
	}
	return json.Marshal(scheduledValidationRecord{ScheduledValidationReport: retained.ScheduledValidationReport, RecordedAt: retained.RecordedAt})
}

func (s *Service) readHealthScheduledRecords(ctx context.Context, organization tracker.OrganizationID, project tracker.ProjectID, now time.Time) (healthScheduledValidation, error) {
	var history healthstore.ScheduledHistory
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(healthstore.ScheduledReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		var err error
		history, err = owner.HealthScheduledHistory(ctx, project, now)
		return err
	})
	if err != nil {
		return healthScheduledValidation{}, err
	}
	out := healthScheduledValidation{FirstReport: history.FirstReport, LastGreen: history.LastGreen}
	var intervals []time.Duration
	for i, report := range history.Reports {
		if i == 0 {
			out.LastReport, out.LastRunURL = report.RecordedAt, report.RunURL
		}
		if report.Conclusion == "failure" && report.RecordedAt.After(out.LastGreen) {
			out.Failures++
		}
		if i > 0 && len(intervals) < 20 {
			if interval := history.Reports[i-1].RecordedAt.Sub(report.RecordedAt); interval > 0 {
				intervals = append(intervals, interval)
			}
		}
	}
	if len(intervals) > 0 {
		slices.Sort(intervals)
		out.Cadence = intervals[len(intervals)/2]
	}
	out.LastFailed = out.Failures > 0
	return out, nil
}
