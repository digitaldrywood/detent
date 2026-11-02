package hubserver

import (
	"context"
	"database/sql"
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

func validateReviewBundle(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error {
	if !artifact.ValidID(bundle.ArtifactID, "artifact") || bundle.Revision < 1 || !artifact.ValidHash(bundle.SHA256, 64) || bundle.HeadSHA != version.HeadSHA {
		return nativePublicInvalid("Review bundle identity does not match the version")
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "SELECT reference_json FROM artifact_references WHERE organization_id=? AND project_id=? AND work_item_id=? AND artifact_id=? AND revision=?", scope.organization, scope.project, change.WorkItemID, bundle.ArtifactID, bundle.Revision).Scan(&raw); err != nil {
		return err
	}
	var ref artifact.Reference
	if err := json.Unmarshal(raw, &ref); err != nil {
		return err
	}
	if ref.SHA256 != bundle.SHA256 || !changerequest.ReviewBundleMatches(version, ref) || !now.Before(ref.ExpiresAt) || ref.Availability != "available" {
		return nativePublicInvalid("Review bundle is unavailable or does not match the version")
	}
	return nil
}

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
	if s.config.TenantRepository != nil {
		ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
		defer cancel()
		scope := nativeRequestScope(c)
		change, err := s.readChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		version, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, c.Param("version"))
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		var rows []eventstore.ViewedFile
		err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			rows, err = r.ViewedFiles(ctx, c.Param("version"), scope.credential.ID, 4097)
			return err
		})
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		if len(rows) > 4096 {
			return s.nativeAPIError(c, corestore.ErrInvalidPage)
		}
		result := make([]tracker.ChangeViewedFile, 0, len(rows))
		for _, row := range rows {
			if row.ChangeID != change.ID || row.VersionID != version.ID || row.PrincipalID != scope.credential.ID {
				return s.nativeAPIError(c, corestore.ErrTenantMismatch)
			}
			result = append(result, tracker.ChangeViewedFile{ManifestSHA256: row.ManifestSHA256, FileSHA256: row.FileSHA256, Viewed: row.Viewed})
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := s.changeViewedFilesCommand(c.Request().Context(), nativeRequestScope(c), c.Param("item"), c.Param("change"), c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
func (s *Service) changeViewedFilesCommand(ctx context.Context, scope nativeScope, item, changeID, versionID string) ([]tracker.ChangeViewedFile, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	change, err := readChange(ctx, s.database.reader, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	version, err := readChangeVersion(ctx, s.database.reader, change.ID, versionID)
	if err != nil {
		return nil, err
	}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT manifest_sha256, file_sha256, viewed FROM change_viewed_files WHERE version_id=? AND principal_id=? ORDER BY manifest_sha256, file_sha256 LIMIT 4096", version.ID, scope.credential.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []tracker.ChangeViewedFile{}
	for rows.Next() {
		var value tracker.ChangeViewedFile
		if err := rows.Scan(&value.ManifestSHA256, &value.FileSHA256, &value.Viewed); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Service) viewChangeFile(c echo.Context) error {
	if s.config.TenantRepository != nil {
		var request tracker.ViewChangeFile
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		return s.viewChangeFileRecord(c, request)
	}
	var request tracker.ViewChangeFile
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	return s.nativeMutation(c, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		return s.viewChangeFileCommand(ctx, tx, scope, c.Param("item"), c.Param("change"), c.Param("version"), request, now)
	})
}

func (s *Service) viewChangeFileCommand(ctx context.Context, tx *sql.Tx, scope nativeScope, item, changeID, versionID string, request tracker.ViewChangeFile, now time.Time) (any, error) {
	change, err := readChange(ctx, tx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	version, err := readChangeVersion(ctx, tx, change.ID, versionID)
	if err != nil {
		return nil, err
	}
	if !artifact.ValidHash(request.FileSHA256, 64) {
		return nil, nativePublicInvalid("An opaque file digest is required")
	}
	if err := validateReviewBundle(ctx, tx, scope, change, version, request.Bundle, now); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM change_viewed_files WHERE version_id=? AND principal_id=? AND NOT (manifest_sha256=? AND file_sha256=?)", version.ID, scope.credential.ID, request.Bundle.SHA256, request.FileSHA256).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 4096 {
		return nil, nativePublicInvalid("Viewed file limit reached for this version")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO change_viewed_files(version_id,principal_id,manifest_sha256,file_sha256,viewed) VALUES(?,?,?,?,?) ON CONFLICT(version_id,principal_id,manifest_sha256,file_sha256) DO UPDATE SET viewed=excluded.viewed", version.ID, scope.credential.ID, request.Bundle.SHA256, request.FileSHA256, request.Viewed)
	return tracker.ChangeViewedFile{ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed}, err
}

func (s *Service) viewChangeFileRecord(c echo.Context, request tracker.ViewChangeFile) error {
	ctx := isolateContentContext(s.runDetailContext(c.Request().Context()))
	scope := nativeRequestScope(c)
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return s.nativeAPIError(c, nativePublicInvalid("An idempotency key of at most 128 bytes is required"))
	}
	if !artifact.ValidHash(request.FileSHA256, 64) {
		return s.nativeAPIError(c, nativePublicInvalid("An opaque file digest is required"))
	}
	hash, err := nativeCommandHash(request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	operation := c.Request().Method + " " + c.Request().URL.EscapedPath()
	replay, found, err := s.changeRecordReceipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if found {
		return c.JSONBlob(http.StatusOK, replay)
	}
	change, err := s.readChangeRecord(ctx, scope, c.Param("item"), c.Param("change"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	versionRecord, err := s.readChangeVersionRecord(ctx, scope.organization, change.ID, c.Param("version"))
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	content := eventstore.NewContent(s.runDetailStore())
	version, err := content.Version(ctx, versionRecord)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	now, err := s.database.currentTime()
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if err := s.validateReviewBundleRecord(ctx, scope, change, version, request.Bundle, now); err != nil {
		return s.nativeAPIError(c, err)
	}
	var artifactReference artifact.Reference
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		artifactReference, err = r.ArtifactReference(ctx, scope.project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	views := tracker.ChangeViewedFile{ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed}
	response, err := json.Marshal(views)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.organization, ActorID: scope.credential.ID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return s.nativeAPIError(c, err)
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
				return s.nativeAPIError(c, readErr)
			}
			if found {
				return c.JSONBlob(http.StatusOK, replay)
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return s.nativeAPIError(c, nativeConflict(change.Revision))
		}
		if err != nil {
			return s.nativeAPIError(c, projectRecordError(err))
		}
		break
	}
	return c.JSONBlob(http.StatusOK, response)
}
