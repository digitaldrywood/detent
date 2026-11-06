package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) postAuditDisposition(c echo.Context) error {
	var request securityaudit.DispositionCommand
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if request.RequestID == "" || len(request.RequestID) > 128 || !request.Confirm || strings.TrimSpace(request.FindingID) == "" || strings.TrimSpace(request.Evidence) == "" || len(request.Evidence) > securityaudit.MaxDispositionEvidenceBytes || request.Key.Repository == "" || request.Key.BaseSHA == "" || request.Key.HeadSHA == "" {
		return s.nativeAPIError(c, nativePublicInvalid("confirmed disposition requires exact source, finding and bounded evidence"))
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope := nativeRequestScope(c)
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var receipt *eventstore.AuditDisposition
	var candidates []eventstore.Evidence
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		reader, ok := r.(eventstore.AuditDispositionReader)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		prior, err := reader.AuditDispositionRequest(ctx, scope.project, scope.credential.ID, request.RequestID)
		if err == nil {
			if err := s.authorizeAuditChange(ctx, r, scope, prior.ChangeID); err != nil {
				return err
			}
			if prior.RequestHash != hash {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
			}
			receipt = &prior
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		candidates, err = reader.AuditReviewCandidates(ctx, scope.project, request.Key, 100)
		if err == nil {
			candidates, err = s.visibleAuditCandidates(ctx, r, scope, candidates)
		}
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if receipt != nil {
		value, err := s.readAuditDisposition(ctx, scope, *receipt)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		return c.JSON(http.StatusOK, value)
	}
	var selected *eventstore.Evidence
	var run securityaudit.Run
	for _, candidate := range candidates {
		raw, err := eventstore.NewContent(s.runDetailStore()).Evidence(ctx, candidate)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		var review tracker.ChangeReview
		if err := json.Unmarshal(raw, &review); err != nil {
			return s.nativeAPIError(c, err)
		}
		if review.SecurityAudit == nil || candidate.VersionID == nil || review.SecurityAudit.VersionID != *candidate.VersionID || review.SecurityAudit.SessionID != review.SecurityAudit.Run.ID {
			continue
		}
		audit := review.SecurityAudit.Run
		if audit.ProjectID != string(scope.project) || (request.Key.ProjectID != "" && request.Key.ProjectID != string(scope.project)) || audit.Repository != request.Key.Repository || audit.BaseSHA != request.Key.BaseSHA || audit.HeadSHA != request.Key.HeadSHA || audit.PRNumber != request.Key.PRNumber {
			continue
		}
		for _, finding := range audit.Findings {
			if finding.ID == request.FindingID {
				selected = &candidate
				run = audit
				break
			}
		}
		break
	}
	if selected == nil || run.ID < 1 {
		return s.nativeAPIError(c, nativeNotFound())
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAuditDisposition(ctx, r, scope, *selected, run, false)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	disposition := securityaudit.Disposition{AuditRunID: run.ID, FindingID: request.FindingID, Status: securityaudit.DispositionFalsePositive, Evidence: strings.TrimSpace(request.Evidence), ServiceIdentity: securityaudit.ServiceIdentity(string(scope.project)), RecordedAt: s.config.now().UTC()}
	raw, err := json.Marshal(disposition)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	ref, err := s.runDetailStore().StoreDetail(ctx, string(scope.organization), raw)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := validateNativeDetailReference(ctx, s.runDetailStore(), scope.organization, ref, raw); err != nil {
		return s.nativeAPIError(c, err)
	}
	row := eventstore.AuditDisposition{ActorID: scope.credential.ID, Organization: scope.organization, Project: scope.project, ChangeID: selected.ChangeID, ReviewSequence: selected.Sequence, Disposition: disposition, Reference: ref}
	row.RequestKey = request.RequestID
	row.RequestHash = hash
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
			return err
		}
		if err := s.authorizeAuditDisposition(ctx, w, scope, *selected, run, true); err != nil {
			return err
		}
		writer, ok := w.(eventstore.AuditDispositionWriter)
		if !ok {
			return nativeRunEventStorageUnavailable()
		}
		row, err = writer.CreateAuditDisposition(ctx, row)
		return err
	})
	if err != nil {
		if errors.Is(err, corestore.ErrRevisionConflict) {
			err = nativeConflict(0)
		}
		return s.nativeAPIError(c, err)
	}
	value, err := s.readAuditDisposition(ctx, scope, row)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) authorizeAuditDisposition(ctx context.Context, r tenantstore.Reader, scope nativeScope, evidence eventstore.Evidence, run securityaudit.Run, lock bool) error {
	if err := s.authorizeAccessScope(ctx, r, scope, true); err != nil {
		return err
	}
	change, err := r.Change(ctx, evidence.ChangeID)
	if err != nil {
		return err
	}
	if change.ProjectID != scope.project || evidence.VersionID == nil {
		return nativeNotFound()
	}
	version, err := r.Version(ctx, change.ID, *evidence.VersionID)
	if err != nil {
		return err
	}
	approved, err := r.ProjectPolicy(ctx, scope.project)
	if err != nil {
		return err
	}
	if version.PolicyRevisionID != approved.PolicyID || version.Repository != run.Repository || version.BaseSHA != run.BaseSHA || version.HeadSHA != run.HeadSHA {
		return tracker.ErrStaleFencingToken
	}
	if scope.credential.Runner.RunnerID == "" {
		if scope.credential.Scope != apiScopeAdmin && scope.credential.HostedRole != "owner" && scope.credential.HostedRole != "admin" {
			return accessstore.ErrDenied
		}
		scope.requireHostedAdmin = true
		return s.authorizeAccessScope(ctx, r, scope, true)
	}
	reader, ok := r.(runnersession.Reader)
	if !ok {
		return nativeRunEventStorageUnavailable()
	}
	session, err := reader.RunnerSession(ctx, scope.project, run.ID)
	if err != nil {
		return err
	}
	if session.Role != "security_audit" || session.IssueID != string(change.WorkItemID) || session.PolicyID != approved.PolicyID {
		return accessstore.ErrDenied
	}
	attempt, err := r.Attempt(ctx, session.AttemptID)
	if err != nil {
		return err
	}
	current, _, err := s.runnerSessionFence(ctx, r, scope, runnersession.Mutation{LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken)}, session.IssueID, lock)
	if err != nil {
		return err
	}
	if current.ID != session.AttemptID {
		return tracker.ErrStaleFencingToken
	}
	return nil
}
