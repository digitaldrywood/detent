package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Attachments (decisions section 17.1). An upload belongs to the actor that
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

// conversationAttachmentRecord is one stored attachment. MessageID is empty
// while the upload is unsent; ExpiresAt is set only then.
type conversationAttachmentRecord struct {
	ID             string
	ConversationID string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	MessageID      string
	PrincipalID    string
	Name           string
	MIME           string
	Size           int64
	ArtifactRef    string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	DeletedAt      *time.Time
}

// conversationAttachmentURL is the hub path that streams an attachment. It is
// the `url` every attachment resource carries, so the client and the runner
// both read the blob through the conversation's audience.
func conversationAttachmentURL(organization tracker.OrganizationID, project tracker.ProjectID, conversationID, attachmentID string) string {
	return "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) +
		"/conversations/" + conversationID + "/attachments/" + attachmentID
}

func (r conversationAttachmentRecord) resource() conversation.Attachment {
	return conversation.Attachment{
		ID: r.ID, Name: r.Name, MIME: r.MIME, Size: r.Size,
		URL: conversationAttachmentURL(r.OrganizationID, r.ProjectID, r.ConversationID, r.ID),
	}
}

// conversationAttachmentRef is the storage key of an upload. It is derived
// from the conversation, the uploading principal and the idempotency key, so
// a retried upload lands on the row it already created instead of a second
// copy, and the unique constraint on the column is the idempotency record.
func conversationAttachmentRef(conversationID, principalID, key string) string {
	sum := sha256.Sum256([]byte(conversationID + "\x00" + principalID + "\x00" + key))
	return "conv:" + conversationID + ":" + hex.EncodeToString(sum[:])
}

func projectAttachments(records []conversationAttachmentRecord) []conversation.Attachment {
	projected := make([]conversation.Attachment, 0, len(records))
	for _, record := range records {
		projected = append(projected, record.resource())
	}
	return projected
}

// Store.

const conversationAttachmentColumns = `a.id, a.conversation_id, c.organization_id, c.project_id, coalesce(a.message_id, ''), a.principal_id,
 a.name, a.mime, a.size, a.artifact_ref, a.created_at, a.expires_at, a.deleted_at`

const conversationAttachmentFrom = ` FROM conversation_attachments a JOIN conversations c ON c.id = a.conversation_id WHERE `

func scanConversationAttachment(row conversationScanner) (conversationAttachmentRecord, error) {
	var record conversationAttachmentRecord
	var created string
	var expires, deleted sql.NullString
	if err := row.Scan(&record.ID, &record.ConversationID, &record.OrganizationID, &record.ProjectID, &record.MessageID,
		&record.PrincipalID, &record.Name, &record.MIME, &record.Size, &record.ArtifactRef, &created, &expires, &deleted); err != nil {
		return record, err
	}
	var err error
	if record.CreatedAt, err = parseTimeValue(created); err != nil {
		return record, fmt.Errorf("decode attachment %s created_at: %w", record.ID, err)
	}
	if record.ExpiresAt, err = conversationParseNullTime(expires); err != nil {
		return record, fmt.Errorf("decode attachment %s expires_at: %w", record.ID, err)
	}
	if record.DeletedAt, err = conversationParseNullTime(deleted); err != nil {
		return record, fmt.Errorf("decode attachment %s deleted_at: %w", record.ID, err)
	}
	return record, nil
}

// readAttachment reads one live attachment of a conversation. A tombstone
// reads as not found: the bytes are gone either way.
func (s *conversationStore) readAttachment(ctx context.Context, query nativeQueryer, conversationID, id string) (conversationAttachmentRecord, error) {
	row := query.QueryRowContext(ctx, "SELECT "+conversationAttachmentColumns+conversationAttachmentFrom+
		"a.id = ? AND a.conversation_id = ? AND a.deleted_at IS NULL", id, conversationID)
	record, err := scanConversationAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record, sql.ErrNoRows
	}
	if err != nil {
		return record, fmt.Errorf("read attachment: %w", err)
	}
	return record, nil
}

// readAttachmentByRef reads the attachment an upload key already created,
// including a tombstone: a replay of a key whose attachment is gone is a
// conflict, not a fresh upload.
func (s *conversationStore) readAttachmentByRef(ctx context.Context, query nativeQueryer, ref string) (conversationAttachmentRecord, bool, error) {
	row := query.QueryRowContext(ctx, "SELECT "+conversationAttachmentColumns+conversationAttachmentFrom+"a.artifact_ref = ?", ref)
	record, err := scanConversationAttachment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, fmt.Errorf("read attachment by reference: %w", err)
	}
	return record, true, nil
}

// insertAttachment stores an upload and its bytes.
func (s *conversationStore) insertAttachment(ctx context.Context, tx *sql.Tx, record conversationAttachmentRecord, content []byte) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_attachments
 (id, conversation_id, message_id, principal_id, name, mime, size, artifact_ref, created_at, expires_at, deleted_at)
 VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		record.ID, record.ConversationID, record.PrincipalID, record.Name, record.MIME, record.Size,
		record.ArtifactRef, conversationTime(record.CreatedAt), conversationNullTime(record.ExpiresAt)); err != nil {
		return fmt.Errorf("insert attachment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO conversation_attachment_blobs (artifact_ref, content) VALUES (?, ?)", record.ArtifactRef, content); err != nil {
		return fmt.Errorf("insert attachment content: %w", err)
	}
	return nil
}

// readAttachmentContent returns the stored bytes. A missing blob for a live
// row cannot happen: the two are written and deleted in one transaction.
func (s *conversationStore) readAttachmentContent(ctx context.Context, query nativeQueryer, ref string) ([]byte, error) {
	var content []byte
	if err := query.QueryRowContext(ctx, "SELECT content FROM conversation_attachment_blobs WHERE artifact_ref = ?", ref).Scan(&content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("read attachment content: %w", err)
	}
	return content, nil
}

// deleteAttachment tombstones an attachment and drops its bytes.
func (s *conversationStore) deleteAttachment(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_attachment_blobs WHERE artifact_ref IN
 (SELECT artifact_ref FROM conversation_attachments WHERE id = ?)`, id); err != nil {
		return fmt.Errorf("delete attachment content: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE conversation_attachments SET deleted_at = ?, expires_at = NULL WHERE id = ?", conversationTime(now), id); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	return nil
}

// bindAttachment binds one unsent attachment to a message. The WHERE clause
// carries the whole rule, so two commands racing for the same attachment
// cannot both win: the second updates no row.
func (s *conversationStore) bindAttachment(ctx context.Context, tx *sql.Tx, conversationID, principalID, attachmentID, messageID string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE conversation_attachments SET message_id = ?, expires_at = NULL
 WHERE id = ? AND conversation_id = ? AND principal_id = ? AND message_id IS NULL AND deleted_at IS NULL`,
		messageID, attachmentID, conversationID, principalID)
	if err != nil {
		return false, fmt.Errorf("bind attachment: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("bind attachment: %w", err)
	}
	return affected == 1, nil
}

// resolveMessageAttachments fills the attachments of every message in one
// query, the way resolveMessageReferences fills references. The hub pool
// holds a single connection, so the caller must have drained its own rows.
func resolveMessageAttachments(ctx context.Context, query nativeQueryer, messages []*conversationMessageRecord) error {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return nil
	}
	encoded, err := marshalNative(ids)
	if err != nil {
		return fmt.Errorf("encode message ids: %w", err)
	}
	rows, err := query.QueryContext(ctx, "SELECT "+conversationAttachmentColumns+conversationAttachmentFrom+
		"a.message_id IN (SELECT value FROM json_each(?)) AND a.deleted_at IS NULL ORDER BY a.message_id, a.created_at, a.id", encoded)
	if err != nil {
		return fmt.Errorf("read message attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byMessage := map[string][]conversationAttachmentRecord{}
	for rows.Next() {
		record, err := scanConversationAttachment(rows)
		if err != nil {
			return fmt.Errorf("read message attachments: %w", err)
		}
		byMessage[record.MessageID] = append(byMessage[record.MessageID], record)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read message attachments: %w", err)
	}
	for _, message := range messages {
		message.Attachments = byMessage[message.ID]
	}
	return nil
}

// listExpiredAttachments returns abandoned uploads: unsent, not deleted and
// past their expiry.
func (s *conversationStore) listExpiredAttachments(ctx context.Context, query nativeQueryer, now time.Time, limit int) ([]conversationAttachmentRecord, error) {
	rows, err := query.QueryContext(ctx, "SELECT "+conversationAttachmentColumns+conversationAttachmentFrom+
		"a.message_id IS NULL AND a.deleted_at IS NULL AND a.expires_at IS NOT NULL AND a.expires_at <= ? ORDER BY a.expires_at LIMIT ?",
		conversationTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	expired := make([]conversationAttachmentRecord, 0, limit)
	for rows.Next() {
		record, err := scanConversationAttachment(rows)
		if err != nil {
			return nil, fmt.Errorf("list expired attachments: %w", err)
		}
		expired = append(expired, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired attachments: %w", err)
	}
	return expired, nil
}

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
		return conversationAttachmentUpload{}, nativeInvalid("A multipart upload with a file part is required")
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
		return conversationAttachmentUpload{}, nativeInvalid("A file part is required")
	}
	if upload.key == "" || len(upload.key) > conversation.MaxCommandKeyBytes {
		return conversationAttachmentUpload{}, nativeInvalid(fmt.Sprintf("An idempotency key of at most %d bytes is required", conversation.MaxCommandKeyBytes))
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
	return &nativeError{Code: "invalid_request", Message: fmt.Sprintf("A file must be at most %d bytes", conversation.MaxAttachmentBytes), status: http.StatusRequestEntityTooLarge}
}

// conversationAttachmentBodyError turns a truncated or oversized body into the
// API error it deserves; anything else is the transport failing.
func conversationAttachmentBodyError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return conversationAttachmentTooLarge()
	}
	return nativeInvalid("The upload could not be read")
}

// uploadConversationAttachment implements POST
// {nativeBase}/conversations/:conversation/attachments.
func (s *Service) uploadConversationAttachment(c echo.Context) error {
	upload, err := readConversationAttachmentUpload(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	name, err := conversation.SanitizeAttachmentName(upload.name)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	if err := conversation.ValidateAttachmentBytes(int64(len(upload.content))); err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	media, err := conversation.SniffAttachment(upload.media, upload.content)
	if err != nil {
		return s.nativeAPIError(c, nativeInvalid(err.Error()))
	}
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	var stored conversationAttachmentRecord
	err = service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, c.Param("conversation"))
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
				return &nativeError{Code: "idempotency_conflict", Message: "Idempotency key has different content", status: http.StatusConflict}
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
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusCreated, conversation.AttachmentUpload{Attachment: stored.resource(), ExpiresAt: stored.ExpiresAt})
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
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	record, err := service.loadConversation(ctx, s.database.db, scope, c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	id := c.Param("attachment")
	if err := conversation.ValidateAttachmentID(id); err != nil {
		return s.nativeAPIError(c, nativeNotFound())
	}
	attachment, err := service.store.readAttachment(ctx, s.database.db, record.ID, id)
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
	}
	content, err := service.store.readAttachmentContent(ctx, s.database.db, attachment.ArtifactRef)
	if err != nil {
		return s.nativeAPIError(c, translateConversationError(err))
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
	scope := nativeRequestScope(c)
	service := s.conversations
	ctx := c.Request().Context()
	id := c.Param("attachment")
	err := service.transact(ctx, func(tx *sql.Tx, now time.Time) error {
		record, err := service.loadConversation(ctx, tx, scope, c.Param("conversation"))
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
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}
