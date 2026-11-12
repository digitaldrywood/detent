package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) validateReviewBundleRecord(ctx context.Context, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error {
	if !artifact.ValidID(bundle.ArtifactID, "artifact") || bundle.Revision < 1 || !artifact.ValidHash(bundle.SHA256, 64) || bundle.HeadSHA != version.HeadSHA {
		return nativePublicInvalid("Review bundle identity does not match the version")
	}
	var ref artifact.Reference
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		ref, err = r.ArtifactReference(ctx, scope.project, change.WorkItemID, bundle.ArtifactID, bundle.Revision)
		return err
	})
	if err != nil {
		return projectRecordError(err)
	}
	if ref.OrganizationID != string(scope.organization) || ref.ProjectID != string(scope.project) || ref.WorkItemID != string(change.WorkItemID) || ref.SHA256 != bundle.SHA256 || !changerequest.ReviewBundleMatches(version, ref) || !now.Before(ref.ExpiresAt) || ref.Availability != "available" {
		return nativePublicInvalid("Review bundle is unavailable or does not match the version")
	}
	return nil
}

func (s *Service) changeViewedFiles(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	result, err := s.readChangeViewedFileRecords(ctx, nativeRequestScope(c), c.Param("item"), c.Param("change"), c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) readChangeViewedFileRecords(ctx context.Context, scope nativeScope, item, changeID, versionID string) ([]tracker.ChangeViewedFile, error) {
	change, err := s.readChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	version, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID)
	if err != nil {
		return nil, projectRecordError(err)
	}
	var rows []eventstore.ViewedFile
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.ViewedFiles(ctx, versionID, scope.credential.ID, 4097)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	if len(rows) > 4096 {
		return nil, corestore.ErrInvalidPage
	}
	result := make([]tracker.ChangeViewedFile, 0, len(rows))
	for _, row := range rows {
		if row.ChangeID != change.ID || row.VersionID != version.ID || row.PrincipalID != scope.credential.ID {
			return nil, corestore.ErrTenantMismatch
		}
		result = append(result, tracker.ChangeViewedFile{ManifestSHA256: row.ManifestSHA256, FileSHA256: row.FileSHA256, Viewed: row.Viewed})
	}
	return result, nil
}

func (s *Service) viewChangeFile(c echo.Context) error {
	var request tracker.ViewChangeFile
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if s.config.TenantRepository == nil {
		return s.nativeAPIError(c, workItemStorageUnavailable())
	}
	return s.viewChangeFileRecord(c, request)
}

func (s *Service) viewChangeFileRecord(c echo.Context, request tracker.ViewChangeFile) error {
	response, err := s.viewChangeFileRecordCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("change"), c.Param("version"), c.Request().Method+" "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) viewChangeFileRecordCommand(ctx context.Context, scope nativeScope, item, changeID, versionID, operation string, request tracker.ViewChangeFile) (json.RawMessage, error) {
	ctx = isolateContentContext(s.runDetailContext(ctx))
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, nativePublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if !artifact.ValidHash(request.FileSHA256, 64) {
		return nil, nativePublicInvalid("An opaque file digest is required")
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return nil, err
	}
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	change, err := s.readChangeRecord(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	versionRecord, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, versionID)
	if err != nil {
		return nil, projectRecordError(err)
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.Version(ctx, versionRecord)
	if err != nil {
		return nil, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	if err := s.validateReviewBundleRecord(ctx, scope, change, version, request.Bundle, now); err != nil {
		return nil, err
	}
	var artifactReference artifact.Reference
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		artifactReference, err = r.ArtifactReference(ctx, scope.project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
		return err
	})
	if err != nil {
		return nil, projectRecordError(err)
	}
	views := tracker.ChangeViewedFile{ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed}
	response, err := json.Marshal(views)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	for {
		err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.credential.ID, operation, request.IdempotencyKey); err == nil {
				if prior.RequestHash != hash {
					return corestore.ErrRevisionConflict
				}
				return corestore.ErrSerializationFailure
			} else if !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			currentVersion, err := w.Version(ctx, change.ID, version.ID)
			if err != nil {
				return err
			}
			if currentVersion != versionRecord {
				return corestore.ErrRevisionConflict
			}
			currentArtifact, err := w.ArtifactReference(ctx, scope.project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
			if err != nil {
				return err
			}
			if currentArtifact != artifactReference || currentArtifact.Availability != "available" || !now.Before(currentArtifact.ExpiresAt) {
				return corestore.ErrRevisionConflict
			}
			stored, err := w.ViewedFiles(ctx, version.ID, scope.credential.ID, 4097)
			if err != nil {
				return err
			}
			if len(stored) > 4096 {
				return corestore.ErrInvalidPage
			}
			alreadyStored := false
			for _, row := range stored {
				alreadyStored = alreadyStored || row.ManifestSHA256 == request.Bundle.SHA256 && row.FileSHA256 == request.FileSHA256
			}
			if !alreadyStored && len(stored) >= 4096 {
				return nativePublicInvalid("Viewed file limit reached for this version")
			}
			_, err = w.SetViewedFile(ctx, eventstore.ViewedFile{OrganizationID: scope.organization, ProjectID: scope.project, ChangeID: change.ID, VersionID: version.ID, PrincipalID: scope.credential.ID, ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed, UpdatedAt: now})
			if err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil, nativeConflict(change.Revision)
		}
		if err != nil {
			return nil, projectRecordError(err)
		}
		break
	}
	return response, nil
}
