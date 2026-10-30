package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
)

// Attachments. An upload belongs to the actor that
// made it and to one conversation. It is unsent until a message command binds
// it, and only while it is unsent may its owner delete it or the sweep expire
// it. Reads follow the conversation's own read rule, so an attachment is
// never more visible than the chat it was dropped into.

const (
	// conversationAttachmentRequestBytes bounds the whole multipart body: the
	// file limit plus room for the framing and the idempotency key.
	conversationAttachmentRequestBytes = conversation.MaxAttachmentBytes + (1 << 20)
	// conversationAttachmentFieldBytes bounds a non-file form field.
	conversationAttachmentFieldBytes = 4096
	// conversationAttachmentSweepBatch bounds one abandoned-upload sweep.
	conversationAttachmentSweepBatch = 200
)

// Store.

// Service.

// loadCommandAttachments resolves the attachments a message command names and
// enforces the rules only the hub knows: the attachment exists in this
// conversation, the actor owns it, it is still unsent and neither deleted nor
// expired, and the set is within the per-message total.
func (c *conversationService) loadCommandAttachments(ctx context.Context, tx *sql.Tx, scope nativeScope, record conversationRecord, ids []string, now time.Time) ([]conversationAttachmentRecord, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	attachments := make([]conversationAttachmentRecord, 0, len(ids))
	var total int64
	for _, id := range ids {
		attachment, err := c.store.readAttachment(ctx, tx, record.ID, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nativeInvalid("Attachment " + id + " is not available on this conversation")
		}
		if err != nil {
			return nil, err
		}
		if attachment.PrincipalID != scope.credential.ID {
			return nil, nativeInvalid("Attachment " + id + " belongs to another principal")
		}
		if attachment.MessageID != "" {
			return nil, nativeInvalid("Attachment " + id + " is already attached to a message")
		}
		if attachment.DeletedAt != nil || (attachment.ExpiresAt != nil && !now.Before(*attachment.ExpiresAt)) {
			return nil, nativeInvalid("Attachment " + id + " is not available on this conversation")
		}
		total += attachment.Size
		attachments = append(attachments, attachment)
	}
	if err := conversation.ValidateAttachmentTotal(total); err != nil {
		return nil, nativeInvalid(err.Error())
	}
	return attachments, nil
}

// bindMessageAttachments binds a resolved set to a freshly appended message.
// A binding that does not take means another command won the attachment
// between the read and the write; the whole command rolls back.
func (c *conversationService) bindMessageAttachments(ctx context.Context, tx *sql.Tx, record conversationRecord, message *conversationMessageRecord, attachments []conversationAttachmentRecord) error {
	for i := range attachments {
		bound, err := c.store.bindAttachment(ctx, tx, record.ID, attachments[i].PrincipalID, attachments[i].ID, message.ID)
		if err != nil {
			return err
		}
		if !bound {
			return nativeInvalid("Attachment " + attachments[i].ID + " is no longer available")
		}
		attachments[i].MessageID = message.ID
		attachments[i].ExpiresAt = nil
	}
	message.Attachments = attachments
	return nil
}

// sweepAttachments deletes abandoned uploads and their bytes, and reports how
// many it removed.
func (c *conversationService) sweepAttachments(ctx context.Context) (int, error) {
	swept := 0
	err := c.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		expired, err := c.store.listExpiredAttachments(ctx, tx, now, conversationAttachmentSweepBatch)
		if err != nil {
			return err
		}
		for _, attachment := range expired {
			if err := c.store.deleteAttachment(ctx, tx, attachment.ID, now); err != nil {
				return err
			}
			swept++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return swept, nil
}

// HTTP.

type conversationAttachmentUpload struct {
	name    string
	media   string
	key     string
	content []byte
}

// readConversationAttachmentUpload streams the multipart body. The parts are
// read rather than parsed into temporary files, so a refused upload never
// reaches the disk. Only the two documented fields are accepted.
func readConversationAttachmentUpload(c echo.Context) (conversationAttachmentUpload, error) {
	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, conversationAttachmentRequestBytes)
	reader, err := request.MultipartReader()
	if err != nil {
		return conversationAttachmentUpload{}, nativePublicInvalid("A multipart upload with a file part is required")
	}
	var upload conversationAttachmentUpload
	seenFile := false
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
		}
		switch part.FormName() {
		case "file":
			if seenFile {
				return conversationAttachmentUpload{}, conversationAttachmentRefusal(part, "An upload carries one file")
			}
			seenFile = true
			upload.name = part.FileName()
			upload.media = part.Header.Get(echo.HeaderContentType)
			// One byte over the limit is enough to know the file is too big.
			upload.content, err = readConversationAttachmentPart(part, conversation.MaxAttachmentBytes+1)
			if err != nil {
				return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
			}
			if int64(len(upload.content)) > conversation.MaxAttachmentBytes {
				return conversationAttachmentUpload{}, conversationAttachmentTooLarge()
			}
		case "idempotency_key":
			value, err := readConversationAttachmentPart(part, conversationAttachmentFieldBytes)
			if err != nil {
				return conversationAttachmentUpload{}, conversationAttachmentBodyError(err)
			}
			upload.key = strings.TrimSpace(string(value))
		default:
			return conversationAttachmentUpload{}, conversationAttachmentRefusal(part, "Field "+part.FormName()+" is not part of an attachment upload")
		}
	}
	if !seenFile {
		return conversationAttachmentUpload{}, nativePublicInvalid("A file part is required")
	}
	if upload.key == "" || len(upload.key) > conversation.MaxCommandKeyBytes {
		return conversationAttachmentUpload{}, nativePublicInvalid(fmt.Sprintf("An idempotency key of at most %d bytes is required", conversation.MaxCommandKeyBytes))
	}
	return upload, nil
}

// readConversationAttachmentPart reads at most limit bytes of a part and
// closes it. The close belongs to the read: a part that will not close has
// not been read cleanly, so its failure joins the read's.
func readConversationAttachmentPart(part *multipart.Part, limit int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(part, limit))
	return content, errors.Join(err, part.Close())
}

// conversationAttachmentRefusal closes the part a refused upload leaves
// unread and returns the refusal. Closing only discards the rest of that
// part, so a close that fails means the body itself is broken and says so
// instead.
func conversationAttachmentRefusal(part *multipart.Part, message string) error {
	if err := part.Close(); err != nil {
		return conversationAttachmentBodyError(err)
	}
	return nativeInvalid(message)
}

func conversationAttachmentTooLarge() error {
	return &nativeError{Code: "invalid_request", Message: fmt.Sprintf("A file must be at most %d bytes", conversation.MaxAttachmentBytes), status: http.StatusRequestEntityTooLarge, publicMessage: true}
}

// conversationAttachmentBodyError turns a truncated or oversized body into the
// API error it deserves; anything else is the transport failing.
func conversationAttachmentBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return conversationAttachmentTooLarge()
	}
	return nativePublicInvalid("The upload could not be read")
}

// uploadConversationAttachment implements POST
// {nativeBase}/conversations/:conversation/attachments.
func (s *Service) uploadConversationAttachment(c echo.Context) error {
	upload, err := readConversationAttachmentUpload(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	value, err := s.commandUploadAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), upload)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusCreated, value)
}

// sameAttachmentUpload reports whether a replayed key describes the very same
// file. A tombstone never matches: the bytes the caller is retrying no longer
// exist, so the key cannot be answered with the row it created.
func (c *conversationService) sameAttachmentUpload(ctx context.Context, tx *sql.Tx, existing conversationAttachmentRecord, name, media string, content []byte) (bool, error) {
	if existing.DeletedAt != nil || existing.Name != name || existing.MIME != media || existing.Size != int64(len(content)) {
		return false, nil
	}
	stored, err := c.store.readAttachmentContent(ctx, tx, existing.ArtifactRef)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bytes.Equal(stored, content), nil
}

// getConversationAttachment implements GET
// {nativeBase}/conversations/:conversation/attachments/:attachment. The
// audience is the conversation's read rule.
func (s *Service) getConversationAttachment(c echo.Context) error {
	attachment, content, err := s.readAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	// The name is a header parameter, never part of the path, so a crafted
	// file name cannot forge a header.
	c.Response().Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Name}))
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, attachment.MIME, content)
}

// deleteConversationAttachment implements DELETE
// {nativeBase}/conversations/:conversation/attachments/:attachment. Only the
// owner may delete, and only while the attachment is unsent.
func (s *Service) deleteConversationAttachment(c echo.Context) error {
	_, err := s.commandDeleteAttachment(c.Request().Context(), nativeRequestScope(c), c.Param("conversation"), c.Param("attachment"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Service) commandDeleteAttachment(ctx context.Context, scope nativeScope, conversationID, id string) (json.RawMessage, error) {
	service := s.conversations
	ctx = newConversationArchiveReads(ctx)
	record, err := service.loadConversation(ctx, service.store.reader, scope, conversationID)
	if err != nil {
		return nil, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	plan := &conversationPublicationPlan{service: service, record: record, original: record, now: now}
	ctx = plan.context(ctx)
	err = service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, conversationID)
		if err != nil {
			return err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		if err := conversation.ValidateAttachmentID(id); err != nil {
			return nativeNotFound()
		}
		attachment, err := service.store.readAttachment(ctx, tx, record.ID, id)
		if err != nil {
			return translateConversationError(err)
		}
		// Someone else's upload is not theirs to see, let alone delete.
		if attachment.PrincipalID != scope.credential.ID {
			return nativeNotFound()
		}
		if attachment.MessageID != "" {
			return conversationForbidden("An attachment that was sent cannot be deleted")
		}
		return service.store.deleteAttachment(ctx, tx, attachment.ID, now)
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(`{"deleted":true}`), nil
}

func (s *Service) commandUploadAttachment(ctx context.Context, scope nativeScope, id string, upload conversationAttachmentUpload) (json.RawMessage, error) {
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
	service := s.conversations
	ctx = newConversationArchiveReads(ctx)
	record, err := service.store.readConversation(ctx, service.store.reader, scope.organization, scope.project, id)
	if err != nil {
		return nil, err
	}
	if err := service.authorizeWrite(ctx, service.store.reader, scope, record); err != nil {
		return nil, err
	}
	existing, found, err := service.store.readAttachmentByRef(ctx, service.store.reader, conversationAttachmentRef(id, scope.credential.ID, upload.key))
	if err != nil {
		return nil, err
	}
	if found && existing.DeletedAt == nil {
		if _, err := service.store.readAttachmentContent(ctx, service.store.reader, existing.ArtifactRef); err != nil {
			return nil, err
		}
	}
	ctx, err = service.store.preparePayload(ctx, string(scope.organization), upload.content)
	if err != nil {
		return nil, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return nil, err
	}
	plan := &conversationPublicationPlan{service: service, record: record, original: record, now: now}
	ctx = plan.context(ctx)
	var stored conversationAttachmentRecord
	err = service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, id)
		if err != nil {
			return err
		}
		if err := service.authorizeWrite(ctx, tx, scope, record); err != nil {
			return err
		}
		if err := service.requireActorAuthority(ctx, tx, scope, now); err != nil {
			return err
		}
		before, err := s.database.hostedConsumption(ctx, tx, now)
		if err != nil {
			return err
		}
		ref := conversationAttachmentRef(record.ID, scope.credential.ID, upload.key)
		existing, found, err := service.store.readAttachmentByRef(ctx, tx, ref)
		if err != nil {
			return err
		}
		if found {
			same, err := service.sameAttachmentUpload(ctx, tx, existing, name, media, upload.content)
			if err != nil {
				return err
			}
			if !same {
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict, publicMessage: true}
			}
			stored = existing
			return nil
		}
		expires := now.Add(conversation.AttachmentTTL)
		stored = conversationAttachmentRecord{
			ID: conversation.NewAttachmentID(), ConversationID: record.ID,
			OrganizationID: record.OrganizationID, ProjectID: record.ProjectID,
			PrincipalID: scope.credential.ID, Name: name, MIME: media, Size: int64(len(upload.content)),
			ArtifactRef: ref, CreatedAt: now, ExpiresAt: &expires,
		}
		if err := service.store.insertAttachment(ctx, tx, stored, upload.content); err != nil {
			return err
		}
		return s.database.checkHostedGrowth(ctx, tx, before, now, false)
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(conversation.AttachmentUpload{Attachment: stored.Resource(), ExpiresAt: stored.ExpiresAt})
}
