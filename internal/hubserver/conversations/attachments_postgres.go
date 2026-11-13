package conversations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func ConversationAttachmentFromStore(a conversationstore.Attachment) ConversationAttachmentRecord {
	return ConversationAttachmentRecord{ID: a.ID, ContentID: a.ContentID, ConversationID: a.ConversationID, OrganizationID: a.OrganizationID, ProjectID: a.ProjectID, MessageID: a.MessageID, PrincipalID: a.PrincipalID, Name: a.Name, MIME: a.MIME, Size: a.Size, ArtifactRef: a.ArtifactRef, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, DeletedAt: a.DeletedAt}
}

func (s *Server) authorizeConversationAttachmentPG(ctx context.Context, r tenantstore.Reader, scope hubauth.Input, current conversationstore.Conversation, write bool) error {
	project, err := r.Project(ctx, scope.Project)
	if err != nil {
		return err
	}
	if project.DeletedAt != nil {
		return hubapi.NotFound()
	}
	if err := s.authorizeAccessScope(ctx, r, scope, write); err != nil {
		return err
	}
	if current.ReadAudience != "" || current.OrganizationID != scope.Organization || current.ProjectID != scope.Project || current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.Credential.ID {
		return hubapi.NotFound()
	}
	return AuthorizeConversationLinkedItemPG(ctx, r, scope, ConversationRecordFromStore(current))
}

func attachmentUploadConflict() error {
	return &hubapi.Error{Code: "idempotency_conflict", Message: "Idempotency key has different content", Status: http.StatusConflict, Public: true}
}

func (s *Server) conversationAttachmentReplayPG(ctx context.Context, scope hubauth.Input, a conversationstore.Attachment, name, media string, content []byte) (json.RawMessage, error) {
	if s.conversations().Store.Contents == nil {
		return nil, ConversationContentUnavailable()
	}
	if a.DeletedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config().Now()) || a.Name != name || a.MIME != media || a.Size != int64(len(content)) {
		return nil, attachmentUploadConflict()
	}
	var object conversationstore.ContentObject
	err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, a.ContentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	stored, err := s.conversations().Store.Contents.ReadContent(ctx, string(scope.Organization), object.Reference)
	if err != nil {
		return nil, err
	}
	if err := s.recheckConversationAttachmentPG(ctx, scope, a, true); err != nil {
		return nil, err
	}
	if !bytes.Equal(stored, content) {
		return nil, attachmentUploadConflict()
	}
	return json.Marshal(conversation.AttachmentUpload{Attachment: ConversationAttachmentFromStore(a).resource(), ExpiresAt: a.ExpiresAt})
}

func (s *Server) commandUploadAttachmentPG(ctx context.Context, scope hubauth.Input, id string, upload ConversationAttachmentUpload) (json.RawMessage, error) {
	name, err := conversation.SanitizeAttachmentName(upload.Name)
	if err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	if err := conversation.ValidateAttachmentBytes(int64(len(upload.Content))); err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	media, err := conversation.SniffAttachment(upload.Media, upload.Content)
	if err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	ref := conversationAttachmentRef(id, scope.Credential.ID, upload.Key)
	var existing conversationstore.Attachment
	var revision int64
	err = s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, true); err != nil {
			return err
		}
		revision = current.Revision
		existing, err = owner.ConversationAttachmentByRef(ctx, scope.Project, id, ref)
		if err == nil {
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	if existing.ID != "" {
		return s.conversationAttachmentReplayPG(ctx, scope, existing, name, media, upload.Content)
	}
	if s.conversations().Store.Contents == nil {
		return nil, ConversationContentUnavailable()
	}
	archive, _, err := attachment.PrepareArchive(string(scope.Organization), 1, bytes.NewReader(upload.Content))
	if err != nil {
		return nil, err
	}
	now := s.config().Now().UTC().Truncate(time.Microsecond)
	stage := artifactstore.ArchiveOwner{OrganizationID: scope.Organization, Kind: "conversation-upload", ID: string(scope.Project) + "/" + ref, CreatedAt: now}
	err = s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(conversationstore.AttachmentWriter)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.LockAttachmentConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, w, scope, current, true); err != nil {
			return err
		}
		existing, err = owner.ConversationAttachmentByRef(ctx, scope.Project, id, ref)
		if err == nil {
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if current.Revision != revision {
			return &ErrConversationRevision{ConversationID: id, Current: current.Revision}
		}
		limit, err := s.attachmentQuotaLimit(ctx, w)
		if err != nil {
			return err
		}
		if err := w.LockArchiveAccounting(ctx); err != nil {
			return err
		}
		object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: ContentArchiveID(string(scope.Organization), archive), Reference: archive, CreatedAt: now})
		if err != nil {
			return err
		}
		stage.ArchiveID = object.ID
		usage, err := w.ChargeArchiveObject(ctx, object.ID, limit)
		if errors.Is(err, corestore.ErrNotFound) {
			return &hubapi.Error{Code: "storage_owner_unavailable", Message: "Attachment accounting is unavailable with PostgreSQL storage", Status: http.StatusServiceUnavailable, Public: true}
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return s.deps.LimitError("collaboration_bytes", limit, usage.Used)
		}
		return w.AddArchiveOwner(ctx, stage)
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	if existing.ID != "" {
		return s.conversationAttachmentReplayPG(ctx, scope, existing, name, media, upload.Content)
	}
	published, err := s.conversations().Store.PublishPayload(ctx, string(scope.Organization), upload.Content)
	if err != nil {
		return nil, err
	}
	if published != archive {
		return nil, ConversationContentUnavailable()
	}
	expires := now.Add(conversation.AttachmentTTL)
	a := conversationstore.Attachment{OrganizationID: scope.Organization, ProjectID: scope.Project, ConversationID: id, ID: conversation.NewAttachmentID(), PrincipalID: scope.Credential.ID, Name: name, MIME: media, Size: int64(len(upload.Content)), ArtifactRef: ref, ContentID: ContentArchiveID(string(scope.Organization), archive), CreatedAt: now, ExpiresAt: &expires}
	replay := false
	err = s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(conversationstore.AttachmentWriter)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.LockAttachmentConversation(ctx, scope.Project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, w, scope, current, true); err != nil {
			return err
		}
		existing, err = owner.ConversationAttachmentByRef(ctx, scope.Project, id, ref)
		if err == nil {
			replay = true
			return w.RemoveArchiveOwner(ctx, stage)
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if current.Revision != revision {
			return &ErrConversationRevision{ConversationID: id, Current: current.Revision}
		}
		if err := w.LockArchiveAccounting(ctx); err != nil {
			return err
		}
		if err := owner.CreateConversationAttachment(ctx, a, conversationstore.ContentObject{OrganizationID: scope.Organization, ID: a.ContentID, Reference: archive, CreatedAt: now}); err != nil {
			return err
		}
		return w.RemoveArchiveOwner(ctx, stage)
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	if replay {
		return s.conversationAttachmentReplayPG(ctx, scope, existing, name, media, upload.Content)
	}
	return json.Marshal(conversation.AttachmentUpload{Attachment: ConversationAttachmentFromStore(a).resource(), ExpiresAt: a.ExpiresAt})
}

func (s *Server) ReadAttachmentPG(ctx context.Context, scope hubauth.Input, conversationID, id string) (ConversationAttachmentRecord, []byte, error) {
	if conversation.ValidateAttachmentID(id) != nil {
		return ConversationAttachmentRecord{}, nil, hubapi.NotFound()
	}
	var a conversationstore.Attachment
	var object conversationstore.ContentObject
	err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.Project, conversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, false); err != nil {
			return err
		}
		a, err = owner.ConversationAttachment(ctx, scope.Project, conversationID, id)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config().Now()) {
			return hubapi.NotFound()
		}
		object, err = r.ConversationContent(ctx, a.ContentID)
		return err
	})
	if err != nil {
		return ConversationAttachmentRecord{}, nil, TranslateConversationError(err)
	}
	if s.conversations().Store.Contents == nil {
		return ConversationAttachmentRecord{}, nil, ConversationContentUnavailable()
	}
	content, err := s.conversations().Store.Contents.ReadContent(ctx, string(scope.Organization), object.Reference)
	if err == nil && int64(len(content)) != a.Size {
		err = attachment.ErrInvalid
	}
	if err == nil {
		err = s.recheckConversationAttachmentPG(ctx, scope, a, false)
	}
	return ConversationAttachmentFromStore(a), content, err
}

func (s *Server) commandDeleteAttachmentPG(ctx context.Context, scope hubauth.Input, conversationID, id string) (json.RawMessage, error) {
	if conversation.ValidateAttachmentID(id) != nil {
		return nil, hubapi.NotFound()
	}
	err := s.config().TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(conversationstore.AttachmentWriter)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.LockAttachmentConversation(ctx, scope.Project, conversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, w, scope, current, true); err != nil {
			return err
		}
		a, err := owner.ConversationAttachment(ctx, scope.Project, conversationID, id)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.PrincipalID != scope.Credential.ID {
			return hubapi.NotFound()
		}
		if a.MessageID != "" {
			return conversationForbidden("An attachment that was sent cannot be deleted")
		}
		return owner.DeleteConversationAttachment(ctx, scope.Project, conversationID, id, s.config().Now().UTC())
	})
	if err != nil {
		return nil, TranslateConversationError(err)
	}
	return json.RawMessage(`{"deleted":true}`), nil
}

func (s *Server) recheckConversationAttachmentPG(ctx context.Context, scope hubauth.Input, previous conversationstore.Attachment, write bool) error {
	err := s.config().TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return ConversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.Project, previous.ConversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, write); err != nil {
			return err
		}
		a, err := owner.ConversationAttachment(ctx, scope.Project, previous.ConversationID, previous.ID)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.ContentID != previous.ContentID || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config().Now()) {
			return hubapi.NotFound()
		}
		return nil
	})
	return TranslateConversationError(err)
}

func loadCommandAttachmentsPG(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input, conversationID string, ids []string, now time.Time) ([]ConversationAttachmentRecord, error) {
	if err := conversation.ValidateAttachmentSelection(ids); err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	owner, ok := reader.(conversationstore.AttachmentReader)
	if !ok {
		return nil, ConversationContentUnavailable()
	}
	var records []ConversationAttachmentRecord
	var total int64
	for _, id := range ids {
		a, err := owner.ConversationAttachment(ctx, scope.Project, conversationID, id)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, hubapi.Invalid("Attachment is not available on this conversation")
		}
		if err != nil {
			return nil, err
		}
		if a.PrincipalID != scope.Credential.ID || a.MessageID != "" || a.DeletedAt != nil || a.ExpiresAt == nil || !now.Before(*a.ExpiresAt) || a.ContentID == "" {
			return nil, hubapi.Invalid("Attachment is not available on this conversation")
		}
		if err := conversation.ValidateAttachmentBytes(a.Size); err != nil {
			return nil, hubapi.Invalid(err.Error())
		}
		object, err := reader.ConversationContent(ctx, a.ContentID)
		if err != nil {
			return nil, err
		}
		if object.Reference.Validate(string(scope.Organization)) != nil || object.Reference.UncompressedSize != a.Size || object.ID != ContentArchiveID(string(scope.Organization), object.Reference) {
			return nil, ConversationContentUnavailable()
		}
		total += a.Size
		records = append(records, ConversationAttachmentFromStore(a))
	}
	if err := conversation.ValidateAttachmentTotal(total); err != nil {
		return nil, hubapi.Invalid(err.Error())
	}
	return records, nil
}

func (c *ConversationService) sweepAttachmentsPG(ctx context.Context) (int, error) {
	organization, err := c.LifecycleOrganization()
	if err != nil {
		return 0, err
	}
	now := c.Server.config().Now().UTC()
	var candidates []conversationstore.Attachment
	if err := c.Server.config().TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return ConversationContentUnavailable()
		}
		var err error
		candidates, err = owner.ExpiredConversationAttachments(ctx, now, conversationAttachmentSweepBatch)
		return err
	}); err != nil {
		return 0, err
	}
	swept := 0
	for _, candidate := range candidates {
		removed := false
		err := c.Server.config().TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(conversationstore.AttachmentWriter)
			if !ok {
				return ConversationContentUnavailable()
			}
			maintenance, ok := w.(conversationstore.AttachmentMaintenanceWriter)
			if !ok {
				return ConversationContentUnavailable()
			}
			if _, err := maintenance.LockRetainedAttachmentConversation(ctx, candidate.ProjectID, candidate.ConversationID); err != nil {
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				return err
			}
			a, err := owner.ConversationAttachment(ctx, candidate.ProjectID, candidate.ConversationID, candidate.ID)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if a.DeletedAt != nil || a.MessageID != "" || a.ExpiresAt == nil || a.ExpiresAt.After(now) {
				return nil
			}
			if err := owner.DeleteConversationAttachment(ctx, a.ProjectID, a.ConversationID, a.ID, now); err != nil {
				return err
			}
			removed = true
			return nil
		})
		if err != nil {
			return swept, err
		}
		if removed {
			swept++
		}
	}
	return swept, nil
}
