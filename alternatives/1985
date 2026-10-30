package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type setProjectIconRequest struct {
	tracker.Mutation
	Icon             projecticon.Icon `json:"icon"`
	Content          []byte           `json:"content_base64,omitempty"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func readProjectIcon(ctx context.Context, query nativeQueryer, scope nativeScope) (projecticon.Icon, error) {
	var icon projecticon.Icon
	err := query.QueryRowContext(ctx, `SELECT coalesce(icon_kind,'automatic'),coalesce(icon_name,''),coalesce(icon_color,''),coalesce(icon_emoji,''),coalesce(icon_image_key,''),coalesce(icon_image_sha256,'') FROM projects WHERE organization_id=? AND id=? AND deleted_at IS NULL`, scope.organization, scope.project).Scan(&icon.Kind, &icon.Name, &icon.Color, &icon.Emoji, &icon.ImageKey, &icon.ImageSHA256)
	if err != nil {
		return icon, err
	}
	if icon.Kind == "image" {
		icon.URL = "/organizations/" + string(scope.organization) + "/api/v2/projects/" + string(scope.project) + "/attachments/" + icon.ImageKey
	}
	return icon, nil
}

func projectIconOverride(icon projecticon.Icon) *projecticon.Icon {
	if icon.Kind == "automatic" {
		return nil
	}
	return &icon
}

func (s *Service) setProjectIcon(c echo.Context) error {
	var request setProjectIconRequest
	if err := decodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := nativeRequestScope(c)
	scope.requireHostedAdmin = true
	raw, err := s.setProjectIconCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) setProjectIconCommand(ctx context.Context, scope nativeScope, request setProjectIconRequest) (json.RawMessage, error) {
	scope.requireHostedAdmin = true
	if scope.credential.Hosted == nil && scope.credential.Scope != apiScopeAdmin {
		return nil, &nativeError{Code: "forbidden", Message: "Project administration is required", status: http.StatusForbidden, publicMessage: true}
	}
	if request.Icon.Kind == "image" {
		normalized, err := projecticon.Normalize(request.Content)
		if err != nil {
			return nil, nativePublicInvalid(err.Error())
		}
		request.Content = normalized
	}
	options := nativeCommandOptions{OperationID: projectOperationID(scope, "PUT", "/icon"), Feature: "collaboration"}
	if raw, found, err := s.nativeCommandReplay(ctx, scope, options.OperationID, request.IdempotencyKey, request); found || err != nil {
		return raw, err
	}
	if request.ExpectedRevision <= 0 {
		return nil, nativePublicInvalid("A positive expected revision is required")
	}
	icon := request.Icon
	if icon.ImageKey != "" || icon.ImageSHA256 != "" || icon.URL != "" || icon.Kind != "image" && len(request.Content) != 0 {
		return nil, nativePublicInvalid("Image references are server owned")
	}
	if err := icon.Validate(); err != nil {
		return nil, nativePublicInvalid(err.Error())
	}
	current, err := readProjectIntegration(ctx, s.database.reader, scope)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, nativeConflict(current.Revision)
	}
	var record *attachment.Metadata
	if icon.Kind == "image" {
		content := request.Content
		store := s.config.Icons
		if s.hostedShared() {
			store = s.config.Hosted.SharedEntry.Icons
		}
		if store == nil {
			return nil, nativePublicInvalid("Image storage is unavailable")
		}
		icon.ImageKey = conversation.NewAttachmentID()
		digest := sha256.Sum256(content)
		icon.ImageSHA256 = hex.EncodeToString(digest[:])
		record = &attachment.Metadata{ID: icon.ImageKey, Name: "project-icon.png", ContentType: "image/png", Size: int64(len(content)), SHA256: icon.ImageSHA256, Width: 128, Height: 128}
		if err := s.accountProjectIcon(ctx, scope, *record); err != nil {
			return nil, err
		}
		if err := store.PutIcon(ctx, string(scope.organization), icon.ImageKey, content); err != nil {
			return nil, s.cleanupProjectIcon(ctx, scope, record.ID, fmt.Errorf("upload project icon: %w", err))
		}
	}
	applied := false
	raw, err := s.executeNativeMutation(ctx, scope, options, request.Mutation, request, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
		applied = true
		current, err := readProjectIntegration(ctx, tx, scope)
		if err != nil {
			return nil, err
		}
		if current.Revision != request.ExpectedRevision {
			return nil, nativeConflict(current.Revision)
		}
		if current.Icon != nil && current.Icon.Kind == "image" {
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET deleted_at=? WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL`, formatHubTime(now), scope.organization, scope.project, current.Icon.ImageKey); err != nil {
				return nil, err
			}
		}
		if record != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE attachments SET retention_class='organization' WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL`, scope.organization, scope.project, record.ID); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET icon_kind=?,icon_name=nullif(?,''),icon_color=nullif(?,''),icon_emoji=nullif(?,''),icon_image_key=nullif(?,''),icon_image_sha256=nullif(?,''),integration_revision=integration_revision+1 WHERE organization_id=? AND id=? AND deleted_at IS NULL`, icon.Kind, icon.Name, icon.Color, icon.Emoji, icon.ImageKey, icon.ImageSHA256, scope.organization, scope.project); err != nil {
			return nil, err
		}
		return readProjectIntegration(ctx, tx, scope)
	})
	if record != nil && (err != nil || !applied) {
		err = s.cleanupProjectIcon(ctx, scope, record.ID, err)
	}
	return raw, err
}

func (s *Service) accountProjectIcon(ctx context.Context, scope nativeScope, record attachment.Metadata) error {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin project icon accounting: %w", err)
	}
	defer tx.Rollback()
	if err := s.recheckHostedMutation(ctx, tx, &scope); err != nil {
		return fmt.Errorf("authorize project icon accounting: %w", err)
	}
	now := s.config.now()
	before, err := s.database.hostedConsumption(ctx, tx, now, "collaboration_bytes", "usage_windows")
	if err != nil {
		return fmt.Errorf("read project icon usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.ID, scope.organization, scope.project, scope.credential.ID, record.Name, record.ContentType, record.Size, record.SHA256, record.Width, record.Height, formatHubTime(now)); err != nil {
		return fmt.Errorf("account project icon: %w", err)
	}
	if err := s.database.checkHostedGrowth(ctx, tx, before, now, false, "collaboration_bytes", "usage_windows"); err != nil {
		return fmt.Errorf("check project icon quota: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit project icon accounting: %w", err)
	}
	return nil
}

func (s *Service) cleanupProjectIcon(ctx context.Context, scope nativeScope, id string, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, deleteErr := s.database.db.ExecContext(cleanup, `UPDATE attachments SET deleted_at=? WHERE organization_id=? AND project_id=? AND id=? AND deleted_at IS NULL`, formatHubTime(s.config.now()), scope.organization, scope.project, id); deleteErr != nil {
		return errors.Join(err, fmt.Errorf("tombstone uncommitted project icon: %w", deleteErr))
	}
	return err
}
