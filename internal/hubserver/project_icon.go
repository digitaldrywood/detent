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
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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

func (s *Service) setProjectIcon(c echo.Context) error {
	var request setProjectIconRequest
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	scope.RequireHostedAdmin = true
	raw, err := s.setProjectIconCommand(c.Request().Context(), scope, request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, raw)
}

func (s *Service) setProjectIconCommand(ctx context.Context, scope hubscope.NativeScope, request setProjectIconRequest) (json.RawMessage, error) {
	scope.RequireHostedAdmin = true
	if scope.Credential.Hosted == nil && scope.Credential.Scope != hubapi.APIScopeAdmin {
		return nil, &hubapi.NativeError{Code: "forbidden", Message: "Project administration is required", Status: http.StatusForbidden, PublicMessage: true}
	}
	if request.Icon.Kind == "image" {
		normalized, err := projecticon.Normalize(request.Content)
		if err != nil {
			return nil, hubapi.NativePublicInvalid(err.Error())
		}
		request.Content = normalized
	}
	options := hubapi.NativeCommandOptions{OperationID: projectOperationID(scope, "PUT", "/icon"), Feature: "collaboration"}
	if raw, found, err := s.nativeCommandReplay(ctx, scope, options.OperationID, request.IdempotencyKey, request); found || err != nil {
		return raw, err
	}
	if request.ExpectedRevision <= 0 {
		return nil, hubapi.NativePublicInvalid("A positive expected revision is required")
	}
	icon := request.Icon
	if icon.ImageKey != "" || icon.ImageSHA256 != "" || icon.URL != "" || icon.Kind != "image" && len(request.Content) != 0 {
		return nil, hubapi.NativePublicInvalid("Image references are server owned")
	}
	if err := icon.Validate(); err != nil {
		return nil, hubapi.NativePublicInvalid(err.Error())
	}
	current, err := s.readProjectRecord(ctx, scope)
	if err != nil {
		return nil, err
	}
	if tracker.Revision(current.Settings.Revision) != request.ExpectedRevision {
		return nil, hubapi.NativeConflict(tracker.Revision(current.Settings.Revision))
	}
	var record *attachment.Metadata
	if icon.Kind == "image" {
		content := request.Content
		store := s.config.Icons
		if s.hostedShared() {
			store = s.config.Hosted.SharedEntry.Icons
		}
		if store == nil {
			return nil, hubapi.NativePublicInvalid("Image storage is unavailable")
		}
		icon.ImageKey = conversation.NewAttachmentID()
		digest := sha256.Sum256(content)
		icon.ImageSHA256 = hex.EncodeToString(digest[:])
		record = &attachment.Metadata{ID: icon.ImageKey, Name: "project-icon.png", ContentType: "image/png", Size: int64(len(content)), SHA256: icon.ImageSHA256, Width: 128, Height: 128}
		if err := s.accountProjectIcon(ctx, scope, *record); err != nil {
			return nil, err
		}
		if err := store.PutIcon(ctx, string(scope.Organization), icon.ImageKey, content); err != nil {
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
		latest, err := w.ProjectRecord(ctx, scope.Project)
		if err != nil {
			return err
		}
		if tracker.Revision(latest.Settings.Revision) != request.ExpectedRevision {
			return hubapi.NativeConflict(tracker.Revision(latest.Settings.Revision))
		}
		if latest.Settings.Icon.Kind == "image" {
			if err := w.DeleteIconAttachment(ctx, scope.Project, latest.Settings.Icon.ImageKey, s.config.now()); err != nil {
				return err
			}
		}
		if err := w.UpdateProjectIcon(ctx, scope.Project, int64(request.ExpectedRevision), icon); err != nil {
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

func (s *Service) accountProjectIcon(ctx context.Context, scope hubscope.NativeScope, record attachment.Metadata) error {
	return s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		return s.createIconAttachment(ctx, w, scope, record)
	})
}

func (s *Service) createIconAttachment(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, record attachment.Metadata) error {
	limit, err := s.attachmentQuotaLimit(ctx, w)
	if err != nil {
		return err
	}
	usage, err := w.AdjustCollaborationBytes(ctx, record.Size, limit)
	if errors.Is(err, corestore.ErrNotFound) {
		return hubapi.AttachmentAccountingUnavailable()
	}
	if err != nil {
		return err
	}
	if !usage.Allowed {
		return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
	}
	record.ProjectID, record.Uploader, record.CreatedAt = string(scope.Project), scope.Credential.ID, s.config.now()
	return w.CreateAttachment(ctx, attachmentstore.Record{OrganizationID: scope.Organization, Metadata: record, Kind: "attachment", Retention: "organization"})
}

func (s *Service) cleanupProjectIcon(ctx context.Context, scope hubscope.NativeScope, id string, err error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	deleteErr := s.config.TenantRepository.WriteTenant(cleanup, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteIconAttachment(cleanup, scope.Project, id, s.config.now())
	})
	if deleteErr != nil {
		return errors.Join(err, fmt.Errorf("tombstone uncommitted project icon: %w", deleteErr))
	}
	return err
}
