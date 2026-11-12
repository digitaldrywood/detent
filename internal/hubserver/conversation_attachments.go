package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/digitaldrywood/detent/internal/tracker"
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

// conversationAttachmentRecord is one stored attachment. MessageID is empty
// while the upload is unsent; ExpiresAt is set only then.
type conversationAttachmentRecord struct {
	ID             string
	ContentID      string
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

// Service.

// sweepAttachments deletes abandoned uploads and their bytes, and reports how
// many it removed.
func (c *conversationService) sweepAttachments(ctx context.Context) (int, error) {
	return c.sweepAttachmentsPG(ctx)
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
	return s.commandDeleteAttachmentPG(ctx, scope, conversationID, id)
}

func (s *Service) commandUploadAttachment(ctx context.Context, scope nativeScope, id string, upload conversationAttachmentUpload) (json.RawMessage, error) {
	return s.commandUploadAttachmentPG(ctx, scope, id, upload)
}
