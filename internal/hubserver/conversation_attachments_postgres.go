package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func conversationAttachmentFromStore(a conversationstore.Attachment) conversationAttachmentRecord {
	return conversationAttachmentRecord{ID: a.ID, ContentID: a.ContentID, ConversationID: a.ConversationID, OrganizationID: a.OrganizationID, ProjectID: a.ProjectID, MessageID: a.MessageID, PrincipalID: a.PrincipalID, Name: a.Name, MIME: a.MIME, Size: a.Size, ArtifactRef: a.ArtifactRef, CreatedAt: a.CreatedAt, ExpiresAt: a.ExpiresAt, DeletedAt: a.DeletedAt}
}

func (s *Service) authorizeConversationAttachmentPG(ctx context.Context, r tenantstore.Reader, scope nativeScope, current conversationstore.Conversation, write bool) error {
	project, err := r.Project(ctx, scope.project)
	if err != nil {
		return err
	}
	if project.DeletedAt != nil {
		return nativeNotFound()
	}
	if err := s.authorizeAccessScope(ctx, r, scope, write); err != nil {
		return err
	}
	if current.ReadAudience != "" || current.OrganizationID != scope.organization || current.ProjectID != scope.project || current.Visibility == conversation.VisibilityPrivate && current.OwnerPrincipalID != scope.credential.ID {
		return nativeNotFound()
	}
	return authorizeConversationLinkedItemPG(ctx, r, scope, conversationRecordFromStore(current))
}

func attachmentUploadConflict() error {
	return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
}

func (s *Service) conversationAttachmentReplayPG(ctx context.Context, scope nativeScope, a conversationstore.Attachment, name, media string, content []byte) (json.RawMessage, error) {
	if s.conversations.store.contents == nil {
		return nil, conversationContentUnavailable()
	}
	if a.DeletedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config.now()) || a.Name != name || a.MIME != media || a.Size != int64(len(content)) {
		return nil, attachmentUploadConflict()
	}
	var object conversationstore.ContentObject
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		object, err = r.ConversationContent(ctx, a.ContentID)
		return err
	})
	if err != nil {
		return nil, err
	}
	stored, err := s.conversations.store.contents.ReadContent(ctx, string(scope.organization), object.Reference)
	if err != nil {
		return nil, err
	}
	if err := s.recheckConversationAttachmentPG(ctx, scope, a, true); err != nil {
		return nil, err
	}
	if !bytes.Equal(stored, content) {
		return nil, attachmentUploadConflict()
	}
	return json.Marshal(conversation.AttachmentUpload{Attachment: conversationAttachmentFromStore(a).resource(), ExpiresAt: a.ExpiresAt})
}

func (s *Service) commandUploadAttachmentPG(ctx context.Context, scope nativeScope, id string, upload conversationAttachmentUpload) (json.RawMessage, error) {
	name, err := conversation.SanitizeAttachmentName(upload.name)
	if err != nil {
		return nil, nativeInvalid(err.Error())
	}
	if err := conversation.ValidateAttachmentBytes(int64(len(upload.content))); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	media, err := conversation.SniffAttachment(upload.media, upload.content)
	if err != nil {
		return nil, nativeInvalid(err.Error())
	}
	ref := conversationAttachmentRef(id, scope.credential.ID, upload.key)
	var existing conversationstore.Attachment
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, true); err != nil {
			return err
		}
		existing, err = owner.ConversationAttachmentByRef(ctx, scope.project, id, ref)
		if err == nil {
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, r)
		if err != nil {
			return err
		}
		used, err := r.CollaborationBytes(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return attachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if int64(len(upload.content)) > limit-used {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: used}
		}
		return nil
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if existing.ID != "" {
		return s.conversationAttachmentReplayPG(ctx, scope, existing, name, media, upload.content)
	}
	if s.conversations.store.contents == nil {
		return nil, conversationContentUnavailable()
	}
	archive, err := s.conversations.store.publishPayload(ctx, string(scope.organization), upload.content)
	if err != nil {
		return nil, err
	}
	now := s.config.now().UTC().Truncate(time.Microsecond)
	expires := now.Add(conversation.AttachmentTTL)
	a := conversationstore.Attachment{OrganizationID: scope.organization, ProjectID: scope.project, ConversationID: id, ID: conversation.NewAttachmentID(), PrincipalID: scope.credential.ID, Name: name, MIME: media, Size: int64(len(upload.content)), ArtifactRef: ref, ContentID: contentArchiveID(string(scope.organization), archive), CreatedAt: now, ExpiresAt: &expires}
	replay := false
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(conversationstore.AttachmentWriter)
		if !ok {
			return conversationContentUnavailable()
		}
		current, err := owner.LockAttachmentConversation(ctx, scope.project, id)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, w, scope, current, true); err != nil {
			return err
		}
		existing, err = owner.ConversationAttachmentByRef(ctx, scope.project, id, ref)
		if err == nil {
			replay = true
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		limit, err := s.attachmentQuotaLimit(ctx, w)
		if err != nil {
			return err
		}
		usage, err := w.AdjustCollaborationBytes(ctx, a.Size, limit)
		if errors.Is(err, corestore.ErrNotFound) {
			return attachmentAccountingUnavailable()
		}
		if err != nil {
			return err
		}
		if !usage.Allowed {
			return &hostedLimitError{Resource: "collaboration_bytes", Allowance: limit, Consumption: usage.Used}
		}
		return owner.CreateConversationAttachment(ctx, a, conversationstore.ContentObject{OrganizationID: scope.organization, ID: a.ContentID, Reference: archive, CreatedAt: now})
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	if replay {
		return s.conversationAttachmentReplayPG(ctx, scope, existing, name, media, upload.content)
	}
	return json.Marshal(conversation.AttachmentUpload{Attachment: conversationAttachmentFromStore(a).resource(), ExpiresAt: a.ExpiresAt})
}

func (s *Service) readAttachmentPG(ctx context.Context, scope nativeScope, conversationID, id string) (conversationAttachmentRecord, []byte, error) {
	if conversation.ValidateAttachmentID(id) != nil {
		return conversationAttachmentRecord{}, nil, nativeNotFound()
	}
	var a conversationstore.Attachment
	var object conversationstore.ContentObject
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.project, conversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, false); err != nil {
			return err
		}
		a, err = owner.ConversationAttachment(ctx, scope.project, conversationID, id)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config.now()) {
			return nativeNotFound()
		}
		object, err = r.ConversationContent(ctx, a.ContentID)
		return err
	})
	if err != nil {
		return conversationAttachmentRecord{}, nil, translateConversationError(err)
	}
	if s.conversations.store.contents == nil {
		return conversationAttachmentRecord{}, nil, conversationContentUnavailable()
	}
	content, err := s.conversations.store.contents.ReadContent(ctx, string(scope.organization), object.Reference)
	if err == nil && int64(len(content)) != a.Size {
		err = attachment.ErrInvalid
	}
	if err == nil {
		err = s.recheckConversationAttachmentPG(ctx, scope, a, false)
	}
	return conversationAttachmentFromStore(a), content, err
}

func (s *Service) commandDeleteAttachmentPG(ctx context.Context, scope nativeScope, conversationID, id string) (json.RawMessage, error) {
	if conversation.ValidateAttachmentID(id) != nil {
		return nil, nativeNotFound()
	}
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(conversationstore.AttachmentWriter)
		if !ok {
			return conversationContentUnavailable()
		}
		current, err := owner.LockAttachmentConversation(ctx, scope.project, conversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, w, scope, current, true); err != nil {
			return err
		}
		a, err := owner.ConversationAttachment(ctx, scope.project, conversationID, id)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.PrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if a.MessageID != "" {
			return conversationForbidden("An attachment that was sent cannot be deleted")
		}
		return owner.DeleteConversationAttachment(ctx, scope.project, conversationID, id, s.config.now().UTC())
	})
	if err != nil {
		return nil, translateConversationError(err)
	}
	return json.RawMessage(`{"deleted":true}`), nil
}

func (s *Service) recheckConversationAttachmentPG(ctx context.Context, scope nativeScope, previous conversationstore.Attachment, write bool) error {
	err := s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
		}
		current, err := owner.AttachmentConversation(ctx, scope.project, previous.ConversationID)
		if err != nil {
			return err
		}
		if err := s.authorizeConversationAttachmentPG(ctx, r, scope, current, write); err != nil {
			return err
		}
		a, err := owner.ConversationAttachment(ctx, scope.project, previous.ConversationID, previous.ID)
		if err != nil {
			return err
		}
		if a.DeletedAt != nil || a.ContentID != previous.ContentID || a.ExpiresAt != nil && !a.ExpiresAt.After(s.config.now()) {
			return nativeNotFound()
		}
		return nil
	})
	return translateConversationError(err)
}

func loadCommandAttachmentsPG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, conversationID string, ids []string, now time.Time) ([]conversationAttachmentRecord, error) {
	if err := conversation.ValidateAttachmentSelection(ids); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	owner, ok := reader.(conversationstore.AttachmentReader)
	if !ok {
		return nil, conversationContentUnavailable()
	}
	var records []conversationAttachmentRecord
	var total int64
	for _, id := range ids {
		a, err := owner.ConversationAttachment(ctx, scope.project, conversationID, id)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil, nativeInvalid("Attachment is not available on this conversation")
		}
		if err != nil {
			return nil, err
		}
		if a.PrincipalID != scope.credential.ID || a.MessageID != "" || a.DeletedAt != nil || a.ExpiresAt == nil || !now.Before(*a.ExpiresAt) || a.ContentID == "" {
			return nil, nativeInvalid("Attachment is not available on this conversation")
		}
		if err := conversation.ValidateAttachmentBytes(a.Size); err != nil {
			return nil, nativeInvalid(err.Error())
		}
		object, err := reader.ConversationContent(ctx, a.ContentID)
		if err != nil {
			return nil, err
		}
		if object.Reference.Validate(string(scope.organization)) != nil || object.Reference.UncompressedSize != a.Size || object.ID != contentArchiveID(string(scope.organization), object.Reference) {
			return nil, conversationContentUnavailable()
		}
		total += a.Size
		records = append(records, conversationAttachmentFromStore(a))
	}
	if err := conversation.ValidateAttachmentTotal(total); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	return records, nil
}

func (c *conversationService) sweepAttachmentsPG(ctx context.Context) (int, error) {
	organization, err := c.lifecycleOrganization()
	if err != nil {
		return 0, err
	}
	now := c.server.config.now().UTC()
	var candidates []conversationstore.Attachment
	if err := c.server.config.TenantRepository.ReadTenant(ctx, organization, corestore.Background, func(r tenantstore.Reader) error {
		owner, ok := r.(conversationstore.AttachmentReader)
		if !ok {
			return conversationContentUnavailable()
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
		err := c.server.config.TenantRepository.WriteTenant(ctx, organization, corestore.Background, func(w tenantstore.Writer) error {
			owner, ok := w.(conversationstore.AttachmentWriter)
			if !ok {
				return conversationContentUnavailable()
			}
			maintenance, ok := w.(conversationstore.AttachmentMaintenanceWriter)
			if !ok {
				return conversationContentUnavailable()
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
