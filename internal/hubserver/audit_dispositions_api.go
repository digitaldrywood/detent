package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		if review.SecurityAudit == nil {
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
		change, err := w.Change(ctx, row.ChangeID)
		if err != nil {
			return err
		}
		if change.ProjectID != scope.project {
			return nativeNotFound()
		}
		if err := s.authorizeAuditChange(ctx, w, scope, row.ChangeID); err != nil {
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
