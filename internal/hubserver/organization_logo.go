package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type organizationLogo struct {
	ImageKey    string           `json:"image_key,omitempty"`
	ImageSHA256 string           `json:"image_sha256,omitempty"`
	URL         string           `json:"url,omitempty"`
	Revision    tracker.Revision `json:"revision,string"`
}

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
	logo, err := readOrganizationLogo(c.Request().Context(), s.database.reader, scope.organization)
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

func (s *Service) setOrganizationLogoCommand(ctx context.Context, scope nativeScope, request setOrganizationLogoRequest) (json.RawMessage, error) {
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

	current, err := readOrganizationLogo(ctx, s.database.reader, scope.organization)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, nativeConflict(current.Revision)
	}

	logo := organizationLogo{}
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
		if err := store.PutIcon(ctx, string(scope.organization), logo.ImageKey, request.Content); err != nil {
			return nil, err
		}
	}
	return s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		current, err := readOrganizationLogo(ctx, tx, scope.organization)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if !request.Remove {
			before, err := s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes", "usage_windows")
			if err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,uploader,name,content_type,size,sha256,width,height,created_at,retention_class) VALUES(?,?,?,'organization-logo.png','image/png',?,?,128,128,?,'organization')`, logo.ImageKey, scope.organization, scope.credential.ID, len(request.Content), logo.ImageSHA256, formatHubTime(now)); err != nil {
				return nil, err
			}
			if err := s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes", "usage_windows"); err != nil {
				return nil, err
			}
		}

		if current.ImageKey != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET deleted_at=? WHERE organization_id=? AND project_id IS NULL AND id=? AND deleted_at IS NULL`, formatHubTime(now), scope.organization, current.ImageKey); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE organizations SET logo_image_key=nullif(?,''),logo_image_sha256=nullif(?,''),logo_revision=logo_revision+1 WHERE id=?`, logo.ImageKey, logo.ImageSHA256, scope.organization); err != nil {
			return nil, err
		}
		return readOrganizationLogo(ctx, tx, scope.organization)
	})
}

func (s *Service) getCurrentOrganizationLogoAttachment(c echo.Context) error {
	scope, err := s.organizationLogoScope(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	logo, err := readOrganizationLogo(c.Request().Context(), s.database.reader, scope.organization)
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
 record, err := scanCloudAttachment(s.database.reader.QueryRowContext(c.Request().Context(), `SELECT `+cloudAttachmentColumns+` FROM attachments WHERE organization_id=? AND project_id IS NULL AND id=? AND kind='attachment' AND deleted_at IS NULL AND id=(SELECT logo_image_key FROM organizations WHERE id=?)`, scope.organization, id, scope.organization))
	if err != nil {
		if err == sql.ErrNoRows {
			err = nativeNotFound()
		}
		return s.nativeAPIError(c, err)
	}
	record.AuthorizedPrincipal = scope.credential.ID
	return c.JSON(http.StatusOK, record)
}
