package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	if icon.Kind == "" || icon.Kind == "automatic" {
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
	current, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if tracker.Revision(current.Settings.Revision) != request.ExpectedRevision {
		return nil, nativeConflict(tracker.Revision(current.Settings.Revision))
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
	current.Settings.Icon = icon
	current.Settings.Revision++
	value := projectIntegrationRecord(current)
	applied := false
	raw, err := s.tenantReceiptMutation(ctx, scope, options.OperationID, request.Mutation, request, value, func(w tenantstore.Writer) error {
		if err := s.database.requireHostedFeatureRecord(ctx, w, "collaboration", s.config.now()); err != nil {
			return err
		}
		latest, err := w.ProjectRecord(ctx, scope.project)
		if err != nil {
			return err
		}
		if tracker.Revision(latest.Settings.Revision) != request.ExpectedRevision {
			return nativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		if latest.Settings.Icon.Kind == "image" {
			if err := w.DeleteIconAttachment(ctx, scope.project, latest.Settings.Icon.ImageKey, s.config.now()); err != nil {
				return err
			}
		}
		if err := w.UpdateProjectIcon(ctx, scope.project, int64(request.ExpectedRevision), icon); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if record != nil && (err != nil || !applied) {
		err = s.cleanupProjectIcon(ctx, scope, record.ID, err)
	}
	return raw, err
}

func (s *Service) accountProjectIcon(ctx context.Context, scope nativeScope, record attachment.Metadata) error {
	return s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return s.createIconAttachment(ctx, w, scope, record)
	})
}

func (s *Service) createIconAttachment(ctx context.Context, w tenantstore.Writer, scope nativeScope, record attachment.Metadata) error {
	limit, err := s.attachmentQuotaLimit(ctx, w)
	if err != nil {
		return err
	}
	usage, err := w.AdjustCollaborationBytes(ctx, record.Size, limit)
	if errors.Is(err, corestore.ErrNotFound) {
		return attachmentAccountingUnavailable()
	}
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
	}
	record.ProjectID, record.Uploader, record.CreatedAt = string(scope.project), scope.credential.ID, s.config.now()
	return w.CreateAttachment(ctx, attachmentstore.Record{OrganizationID: scope.organization, Metadata: record, Kind: "attachment", Retention: "organization"})
}

func (s *Service) cleanupProjectIcon(ctx context.Context, scope nativeScope, id string, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	deleteErr := s.config.TenantRepository.WriteTenant(cleanup, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteIconAttachment(cleanup, scope.project, id, s.config.now())
	})
	if deleteErr != nil {
		return errors.Join(err, fmt.Errorf("tombstone uncommitted project icon: %w", deleteErr))
	}
	return err
}
