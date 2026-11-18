package orgsettings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projecticon"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Logo = accessstore.Logo

type LogoRequest struct {
	tracker.Mutation
	Content          []byte           `json:"content_base64,omitempty"`
	Remove           bool             `json:"remove,omitempty"`
	ExpectedRevision tracker.Revision `json:"expected_revision,string"`
}

func LogoOperationID(organization tracker.OrganizationID) string {
	return "PUT /api/v2/organizations/" + string(organization) + "/logo"
}

func logoURL(organization tracker.OrganizationID, key string) string {
	return "/organizations/" + string(organization) + "/api/v2/attachments/" + key
}

func (s *Service) SetLogo(ctx context.Context, scope hubapi.Scope, request LogoRequest) (result json.RawMessage, resultErr error) {
	if !s.deps.Manager {
		return nil, operatortool.ErrAccessDenied
	}
	if request.ExpectedRevision <= 0 || request.Remove && len(request.Content) != 0 {
		return nil, hubapi.PublicInvalid("Choose an image or remove the logo, with a positive expected revision")
	}
	if !request.Remove {
		content, err := projecticon.Normalize(request.Content)
		if err != nil {
			return nil, hubapi.PublicInvalid(err.Error())
		}
		request.Content = content
	}
	operation := LogoOperationID(scope.Organization)
	if raw, found, err := s.deps.Replay(ctx, operation, request.IdempotencyKey, request); found || err != nil {
		return raw, err
	}
	current, err := s.ReadLogo(ctx, scope.Organization)
	if err != nil {
		return nil, err
	}
	if current.Revision != request.ExpectedRevision {
		return nil, hubapi.Conflict(current.Revision)
	}
	logo := Logo{Revision: request.ExpectedRevision + 1}
	if !request.Remove {
		store := s.deps.Icons
		if store == nil {
			return nil, hubapi.PublicInvalid("Image storage is unavailable")
		}
		logo.ImageKey = conversation.NewAttachmentID()
		digest := sha256.Sum256(request.Content)
		logo.ImageSHA256 = hex.EncodeToString(digest[:])
		logo.URL = logoURL(scope.Organization, logo.ImageKey)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			err := s.deps.Repository.ReadTenant(cleanup, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { _, err := r.Attachment(cleanup, "", logo.ImageKey); return err })
			if errors.Is(err, corestore.ErrNotFound) {
				resultErr = errors.Join(resultErr, store.DeleteIcon(cleanup, string(scope.Organization), logo.ImageKey))
			} else if err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if err := store.PutIcon(ctx, string(scope.Organization), logo.ImageKey, request.Content); err != nil {
			return nil, err
		}
	}
	return s.deps.Mutate(ctx, operation, request.Mutation, request, logo, func(w tenantstore.Writer) error {
		if err := s.deps.RequireFeature(ctx, w, "collaboration", s.deps.Now()); err != nil {
			return err
		}
		current, err := w.OrganizationLogo(ctx)
		if err != nil {
			return err
		}
		if current.Revision != request.ExpectedRevision {
			return hubapi.Conflict(current.Revision)
		}
		if !request.Remove {
			metadata := attachment.Metadata{ID: logo.ImageKey, Name: "organization-logo.png", ContentType: "image/png", Size: int64(len(request.Content)), SHA256: logo.ImageSHA256, Width: 128, Height: 128}
			if err := s.deps.CreateIcon(ctx, w, metadata); err != nil {
				return err
			}
		}
		if current.ImageKey != "" {
			if err := w.DeleteIconAttachment(ctx, "", current.ImageKey, s.deps.Now()); err != nil {
				return err
			}
		}
		return w.UpdateOrganizationLogo(ctx, logo, request.ExpectedRevision)
	})
}

func (s *Service) ReadLogo(ctx context.Context, organization tracker.OrganizationID) (Logo, error) {
	var logo Logo
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error { var err error; logo, err = r.OrganizationLogo(ctx); return err })
	if logo.ImageKey != "" {
		logo.URL = logoURL(organization, logo.ImageKey)
	}
	return logo, s.deps.RecordError(err)
}

func (s *Service) LogoAttachment(ctx context.Context, scope hubapi.Scope, id string) (attachment.Metadata, error) {
	var record attachmentstore.Record
	err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := scope.Authorize(ctx, r, false); err != nil {
			return err
		}
		logo, err := r.OrganizationLogo(ctx)
		if err != nil {
			return err
		}
		if id == "" || id != logo.ImageKey {
			return hubapi.NotFound()
		}
		record, err = r.Attachment(ctx, "", id)
		if err != nil {
			return err
		}
		if record.Metadata.DeletedAt != nil || record.Kind != "attachment" {
			return hubapi.NotFound()
		}
		return nil
	})
	if err != nil {
		return attachment.Metadata{}, s.deps.RecordError(err)
	}
	record.Metadata.AuthorizedPrincipal = scope.CredentialID
	return record.Metadata, nil
}
