package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type scheduledValidationRecord struct {
	gate.ScheduledValidationReport
	RecordedAt time.Time `json:"recorded_at"`
}

func (s *Service) recordScheduledValidation(ctx context.Context, scope nativeScope, request tracker.CreateIssue) (json.RawMessage, error) {
	report := request.ScheduledValidation
	if report == nil || !report.Valid() || request.ScheduledFailure != nil || request.ChallengeToken != "" || len(request.NotDuplicateOf) != 0 || request.Type != "" || request.ReportContext != nil || request.Title != "" || request.Body != "" || request.State != "" || request.Priority != nil || len(request.Labels) != 0 || len(request.Assignees) != 0 || request.Provenance != nil || request.GitHubIssueURL != "" {
		return nil, nativePublicInvalid("Invalid scheduled validation report")
	}
	if scope.credential.Scope == apiScopeWorker || scope.credential.HostedRole == "reporter" {
		return nil, &nativeError{Code: "insufficient_scope", Message: "Scheduled reporting requires operator authority", status: http.StatusForbidden, publicMessage: true}
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if s.config.TenantRepository == nil {
		return nil, workItemStorageUnavailable()
	}
	scope.action = ""
	if err := s.recheckProjectRecordMutation(ctx, scope); err != nil {
		return nil, err
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	operation := "POST /api/v2/organizations/" + string(scope.organization) + "/projects/" + string(scope.project) + "/work-items"
	var previous *eventstore.Receipt
	planned := healthstore.ScheduledReport{OrganizationID: scope.organization, ProjectID: scope.project, ScheduledValidationReport: *report, RecordedAt: s.config.now().UTC().Truncate(time.Microsecond)}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeWorkItemRecordScope(ctx, r, scope); err != nil {
			return err
		}
		receipt, err := r.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if receipt.RequestHash != hash {
				return nativeConflict(0)
			}
			previous = &receipt
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		project, err := r.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		repository := project.Settings.CheckoutRepository
		if repository == "" {
			repository = project.Settings.Repository
		}
		if project.Core.Profile != "native" || !strings.EqualFold(repository, report.Repository) {
			return nativePublicInvalid("Scheduled validation repository does not match the project")
		}
		owner, ok := r.(healthstore.ScheduledReportReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		retained, err := owner.HealthScheduledReport(ctx, scope.project, report.Repository, report.RunID, report.RunAttempt)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if retained.ScheduledValidationReport != *report {
			return nativePublicInvalid("Scheduled validation run already has a different report")
		}
		planned = retained
		return nil
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	if previous != nil {
		return content.Receipt(ctx, *previous)
	}
	raw, err := json.Marshal(scheduledValidationRecord{ScheduledValidationReport: planned.ScheduledValidationReport, RecordedAt: planned.RecordedAt})
	if err != nil {
		return nil, err
	}
	now := s.config.now().UTC()
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now, ResponseReplayUntilNs: now.Add(receiptstore.ReplayRetention).UnixNano()}, raw)
	if err != nil {
		return nil, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		if err := s.authorizeWorkItemRecordScope(ctx, w, scope); err != nil {
			return err
		}
		stored, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey)
		if err == nil {
			if stored.RequestHash != hash {
				return nativeConflict(0)
			}
			previous = &stored
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if s.config.Hosted != nil {
			if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", now); err != nil {
				return err
			}
		}
		owner, ok := w.(healthstore.ScheduledWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		retained, err := owner.RecordHealthScheduledReport(ctx, planned)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nativePublicInvalid("Scheduled validation run already has a different report")
		}
		if err != nil {
			return err
		}
		if !retained.RecordedAt.Equal(planned.RecordedAt) {
			return nativeConflict(0)
		}
		if scope.credential.Hosted != nil {
			audit := projectRecordActor(scope).Audit
			audit.Event, audit.Route, audit.Status = "action", operation, http.StatusOK
			audit.RecordedAt, audit.RecordedAtNS = now, now.UnixNano()
			if _, err := w.RecordAccessAudit(ctx, audit); err != nil {
				return err
			}
		}
		_, err = w.PutReceipt(ctx, receipt)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if previous != nil {
		return content.Receipt(ctx, *previous)
	}
	return raw, nil
}
