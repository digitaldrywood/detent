package changefiles

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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) ValidateBundle(ctx context.Context, scope hubapi.Scope, change tracker.ChangeRequest, version tracker.ChangeVersion, bundle tracker.ChangeReviewBundle, now time.Time) error {
	if !artifact.ValidID(bundle.ArtifactID, "artifact") || bundle.Revision < 1 || !artifact.ValidHash(bundle.SHA256, 64) || bundle.HeadSHA != version.HeadSHA {
		return hubapi.PublicInvalid("Review bundle identity does not match the version")
	}
	var ref artifact.Reference
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		ref, err = r.ArtifactReference(ctx, scope.Project, change.WorkItemID, bundle.ArtifactID, bundle.Revision)
		return err
	})
	if err != nil {
		return s.deps.RecordError(err)
	}
	if ref.OrganizationID != string(scope.Organization) || ref.ProjectID != string(scope.Project) || ref.WorkItemID != string(change.WorkItemID) || ref.SHA256 != bundle.SHA256 || !changerequest.ReviewBundleMatches(version, ref) || !now.Before(ref.ExpiresAt) || ref.Availability != "available" {
		return hubapi.PublicInvalid("Review bundle is unavailable or does not match the version")
	}
	return nil
}

func (s *Service) ViewedFiles(c echo.Context) error {
	ctx := c.Request().Context()
	result, err := s.ReadViewedFiles(ctx, s.deps.Scope(c), c.Param("item"), c.Param("change"), c.Param("version"))
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) ReadViewedFiles(ctx context.Context, scope hubapi.Scope, item, changeID, versionID string) ([]tracker.ChangeViewedFile, error) {
	change, err := s.deps.ReadChange(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	version, err := s.deps.ReadVersion(ctx, scope.Organization, change.ID, versionID)
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	var rows []eventstore.ViewedFile
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		rows, err = r.ViewedFiles(ctx, versionID, scope.CredentialID, 4097)
		return err
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	if len(rows) > 4096 {
		return nil, corestore.ErrInvalidPage
	}
	result := make([]tracker.ChangeViewedFile, 0, len(rows))
	for _, row := range rows {
		if row.ChangeID != change.ID || row.VersionID != version.ID || row.PrincipalID != scope.CredentialID {
			return nil, corestore.ErrTenantMismatch
		}
		result = append(result, tracker.ChangeViewedFile{ManifestSHA256: row.ManifestSHA256, FileSHA256: row.FileSHA256, Viewed: row.Viewed})
	}
	return result, nil
}

func (s *Service) ViewFile(c echo.Context) error {
	var request tracker.ViewChangeFile
	if err := hubapi.DecodeJSON(c, &request); err != nil {
		return s.deps.InvalidRequest(c, err)
	}
	if s.deps.Repository == nil {
		return s.deps.Error(c, hubapi.WorkItemStorageUnavailable())
	}
	return s.ViewFileRecord(c, request)
}

func (s *Service) ViewFileRecord(c echo.Context, request tracker.ViewChangeFile) error {
	response, err := s.ViewFileCommand(c.Request().Context(), s.deps.Scope(c), c.Param("item"), c.Param("change"), c.Param("version"), c.Request().Method+" "+c.Request().URL.EscapedPath(), request)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func (s *Service) ViewFileCommand(ctx context.Context, scope hubapi.Scope, item, changeID, versionID, operation string, request tracker.ViewChangeFile) (json.RawMessage, error) {
	ctx = s.deps.Context(ctx)
	if strings.TrimSpace(request.IdempotencyKey) == "" || len(request.IdempotencyKey) > 128 {
		return nil, hubapi.PublicInvalid("An idempotency key of at most 128 bytes is required")
	}
	if !artifact.ValidHash(request.FileSHA256, 64) {
		return nil, hubapi.PublicInvalid("An opaque file digest is required")
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		return nil, err
	}
	replay, found, err := s.deps.Receipt(ctx, scope, operation, request.IdempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	if found {
		return replay, nil
	}
	change, err := s.deps.ReadChange(ctx, scope, item, changeID)
	if err != nil {
		return nil, err
	}
	versionRecord, err := s.deps.ReadVersion(ctx, scope.Organization, change.ID, versionID)
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	content := eventstore.NewContent(s.deps.Details())
	version, err := content.Version(ctx, versionRecord)
	if err != nil {
		return nil, err
	}
	now, err := s.deps.CurrentTime()
	if err != nil {
		return nil, err
	}
	if err := s.ValidateBundle(ctx, scope, change, version, request.Bundle, now); err != nil {
		return nil, err
	}
	var artifactReference artifact.Reference
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		artifactReference, err = r.ArtifactReference(ctx, scope.Project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
		return err
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	views := tracker.ChangeViewedFile{ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed}
	response, err := json.Marshal(views)
	if err != nil {
		return nil, err
	}
	receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: scope.Organization, ActorID: scope.CredentialID, Operation: operation, CommandKey: request.IdempotencyKey, RequestHash: hash, CreatedAt: now}, response)
	if err != nil {
		return nil, err
	}
	for {
		err = s.deps.Repository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := scope.Authorize(ctx, w, true); err != nil {
				return err
			}
			if err := w.LockWorkItemProject(ctx, scope.Project); err != nil {
				return err
			}
			if prior, err := w.Receipt(ctx, scope.CredentialID, operation, request.IdempotencyKey); err == nil {
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
			currentArtifact, err := w.ArtifactReference(ctx, scope.Project, change.WorkItemID, request.Bundle.ArtifactID, request.Bundle.Revision)
			if err != nil {
				return err
			}
			if currentArtifact != artifactReference || currentArtifact.Availability != "available" || !now.Before(currentArtifact.ExpiresAt) {
				return corestore.ErrRevisionConflict
			}
			stored, err := w.ViewedFiles(ctx, version.ID, scope.CredentialID, 4097)
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
				return hubapi.PublicInvalid("Viewed file limit reached for this version")
			}
			_, err = w.SetViewedFile(ctx, eventstore.ViewedFile{OrganizationID: scope.Organization, ProjectID: scope.Project, ChangeID: change.ID, VersionID: version.ID, PrincipalID: scope.CredentialID, ManifestSHA256: request.Bundle.SHA256, FileSHA256: request.FileSHA256, Viewed: request.Viewed, UpdatedAt: now})
			if err != nil {
				return err
			}
			_, err = w.PutReceipt(ctx, receipt)
			return err
		})
		if errors.Is(err, corestore.ErrSerializationFailure) || errors.Is(err, corestore.ErrUniqueViolation) {
			replay, found, readErr := s.deps.Receipt(ctx, scope, operation, request.IdempotencyKey, hash)
			if readErr != nil {
				return nil, readErr
			}
			if found {
				return replay, nil
			}
			continue
		}
		if errors.Is(err, corestore.ErrRevisionConflict) {
			return nil, hubapi.Conflict(change.Revision)
		}
		if err != nil {
			return nil, s.deps.RecordError(err)
		}
		break
	}
	return response, nil
}
