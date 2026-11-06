package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationLogo = accessstore.Logo

type setOrganizationLogoRequest struct {
	tracker.Mutation
	Content          []byte           `json:"content_base64,omitempty"`
	Remove           bool             `json:"remove,omitempty"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func readOrganizationLogo(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID) (organizationLogo, error) {
	var logo organizationLogo
	err := query.QueryRowContext(ctx, `SELECT coalesce(logo_image_key,''),coalesce(logo_image_sha256,''),logo_revision FROM organizations WHERE id=?`, organization).Scan(&logo.ImageKey, &logo.ImageSHA256, &logo.Revision)
	if logo.ImageKey != "" {
		logo.URL = "/organizations/" + string(organization) + "/api/v2/attachments/" + logo.ImageKey
	}
	return logo, err
}

func (s *Service) organizationLogoScope(c echo.Context) (nativeScope, error) {
	if scope, ok := c.Get("native_scope").(nativeScope); ok {
		return scope, nil
	}
	return s.organizationModelSelectionScope(c)
}

func (s *Service) getOrganizationLogo(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	logo, err := s.readOrganizationLogoRecord(c.Request().Context(), scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, logo)
}

func (s *Service) setOrganizationLogo(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	var request setOrganizationLogoRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	raw, err := s.setOrganizationLogoCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func organizationLogoOperationID(scope nativeScope) string {
	return "PUT /api/v2/organizations/" + string(scope.organization) + "/logo"
}

func (s *Service) setOrganizationLogoCommand(ctx context.Context, scope nativeScope, request setOrganizationLogoRequest) (result json.RawMessage, resultErr error) {
	scope.project, scope.requireHostedAdmin = "", true
	if !canManageProjectSecrets(scope.credential) {
		return nil, operatortool.ErrAccessDenied
	}
	if request.ExpectedRevision <= 0 || request.Remove && len(request.Content) != 0 {
		return nil, nativePublicInvalid("Choose an image or remove the logo, with a positive expected revision")
	}
	if !request.Remove {
		content, err := projecticon.Normalize(request.Content)
		if err != nil {
			return nil, nativePublicInvalid(err.Error())
		}
		request.Content = content
	}
	options := nativeCommandOptions{OperationID: organizationLogoOperationID(scope), Feature: "collaboration"}
	if raw, found, err := s.nativeCommandReplay(ctx, scope, options.OperationID, request.IdempotencyKey, request); found || err != nil {
		return raw, err
	}

	current, err := s.readOrganizationLogoRecord(ctx, scope.organization)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, nativeConflict(current.Revision)
	}
	logo := organizationLogo{Revision: request.ExpectedRevision + 1}
	if !request.Remove {
		store := s.config.Icons
		if s.hostedShared() {
			store = s.config.Hosted.SharedEntry.Icons
		}
		if store == nil {
			return nil, nativePublicInvalid("Image storage is unavailable")
		}
		logo.ImageKey = conversation.NewAttachmentID()
		digest := sha256.Sum256(request.Content)
		logo.ImageSHA256 = hex.EncodeToString(digest[:])
		logo.URL = "/organizations/" + string(scope.organization) + "/api/v2/attachments/" + logo.ImageKey
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			err := s.config.TenantRepository.ReadTenant(cleanup, scope.organization, corestore.Serving, func(r tenantstore.Reader) error { _, err := r.Attachment(cleanup, "", logo.ImageKey); return err })
			if errors.Is(err, corestore.ErrNotFound) {
				resultErr = errors.Join(resultErr, store.DeleteIcon(cleanup, string(scope.organization), logo.ImageKey))
			} else if err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if err := store.PutIcon(ctx, string(scope.organization), logo.ImageKey, request.Content); err != nil {
			return nil, err
		}
	}
	return s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, logo, func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		current, err := w.OrganizationLogo(ctx)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return nativeConflict(current.Revision)
		}
		if !request.Remove {
			metadata := attachment.Metadata{ID: logo.ImageKey, Name: "organization-logo.png", ContentType: "image/png", Size: int64(len(request.Content)), SHA256: logo.ImageSHA256, Width: 128, Height: 128}
			if err := s.createIconAttachment(ctx, w, scope, metadata); err != nil {
				return err
			}
		}
		if current.ImageKey != "" {
			if err := w.DeleteIconAttachment(ctx, "", current.ImageKey, s.config.now()); err != nil {
				return err
			}
		}
		return w.UpdateOrganizationLogo(ctx, logo, request.ExpectedRevision)
	})
}

func (s *Service) readOrganizationLogoRecord(ctx context.Context, organization tracker.OrganizationID) (organizationLogo, error) {
	var logo organizationLogo
	err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error { var err error; logo, err = r.OrganizationLogo(ctx); return err })
	if logo.ImageKey != "" {
		logo.URL = "/organizations/" + string(organization) + "/api/v2/attachments/" + logo.ImageKey
	}
	return logo, projectRecordError(err)
}

func (s *Service) getCurrentOrganizationLogoAttachment(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	logo, err := s.readOrganizationLogoRecord(c.Request().Context(), scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.organizationLogoAttachment(c, scope, logo.ImageKey)
}

func (s *Service) getOrganizationLogoAttachment(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return s.organizationLogoAttachment(c, scope, c.Param("attachment"))
}

func (s *Service) organizationLogoAttachment(c echo.Context, scope nativeScope, id string) error {
	var record attachmentstore.Record
	err := s.config.TenantRepository.ReadTenant(c.Request().Context(), scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		ctx := c.Request().Context()
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		logo, err := r.OrganizationLogo(ctx)
		if err != nil {
			return err
		}
		if id == "" || id != logo.ImageKey {
			return nativeNotFound()
		}
		record, err = r.Attachment(ctx, "", id)
		if err != nil {
			return err
		}
		if record.Metadata.DeletedAt != nil || record.Kind != "attachment" {
			return nativeNotFound()
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, projectRecordError(err))
	}
	record.Metadata.AuthorizedPrincipal = scope.credential.ID
	return c.JSON(http.StatusOK, record.Metadata)
}
