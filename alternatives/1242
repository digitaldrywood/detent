package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Attachments (decisions section 17.1). The domain owns the identifier
// shape, the accepted media types, the content sniff and the per-message
// limits; the hub owns storage, the endpoints and the audience check.

const attachmentPrefix = "att_"

// Limits on attachments.
const (
	// MaxAttachmentBytes is the largest single upload.
	MaxAttachmentBytes = 20 << 20
	// MaxMessageAttachments is how many attachments one message may carry.
	MaxMessageAttachments = 10
	// MaxAttachmentNameBytes bounds the stored file name.
	MaxAttachmentNameBytes = 255
	// AttachmentTTL is how long an upload that was never sent survives
	// before the sweep deletes it together with its blob.
	AttachmentTTL = 7 * 24 * time.Hour
)

// ErrInvalidAttachment reports an upload or selection that fails validation.
var ErrInvalidAttachment = errors.New("invalid attachment")

// attachmentMediaTypes is the accepted media type allow list, in contract
// order: images the provider takes as image input, then the text types the
// runner hands over as a delimited data block.
var attachmentMediaTypes = []string{
	"image/png", "image/jpeg", "image/gif", "image/webp",
	"text/plain", "text/markdown", "text/csv", "application/json",
}

// NewAttachmentID returns a fresh attachment identifier ("att_" + 32 hex).
func NewAttachmentID() string { return attachmentPrefix + randomHex() }

// ValidateAttachmentID reports whether value is a well-formed attachment
// identifier.
func ValidateAttachmentID(value string) error {
	return validateID("attachment", attachmentPrefix, value)
}

// AttachmentMediaTypes returns the accepted media types in contract order.
// The result is a copy, so a caller cannot rewrite the allow list.
func AttachmentMediaTypes() []string { return slices.Clone(attachmentMediaTypes) }

// AttachmentMediaAllowed reports whether media is an accepted media type.
// The value is normalized first, so a parameterized header is accepted.
func AttachmentMediaAllowed(media string) bool {
	return slices.Contains(attachmentMediaTypes, NormalizeMediaType(media))
}

// NormalizeMediaType lowercases a media type and drops its parameters, so
// "TEXT/PLAIN; charset=utf-8" and "text/plain" are the same type. A value
// that is not a media type at all normalizes to itself, trimmed and
// lowercased, which the allow list then rejects.
func NormalizeMediaType(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(trimmed); err == nil {
		return strings.ToLower(strings.TrimSpace(parsed))
	}
	base, _, _ := strings.Cut(trimmed, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// SniffAttachment reports the media type to store for an upload. declared is
// what the client said the file is; an empty declaration falls back to the
// sniff. The content is read rather than trusted: a declared type outside
// the allow list, an empty file, or content that does not match the declared
// type is ErrInvalidAttachment.
func SniffAttachment(declared string, content []byte) (string, error) {
	if len(content) == 0 {
		return "", fmt.Errorf("%w: the file is empty", ErrInvalidAttachment)
	}
	detected := NormalizeMediaType(http.DetectContentType(content))
	media := NormalizeMediaType(declared)
	if media == "" {
		media = detected
	}
	if !AttachmentMediaAllowed(media) {
		return "", fmt.Errorf("%w: %q is not an accepted media type", ErrInvalidAttachment, media)
	}
	switch {
	case strings.HasPrefix(media, "image/"):
		// An image is sniffed exactly: the magic bytes name the format, so
		// a renamed file cannot pass as another image.
		if detected != media {
			return "", fmt.Errorf("%w: the content is %q, not %q", ErrInvalidAttachment, detected, media)
		}
	case media == "application/json":
		if !json.Valid(content) || !utf8.Valid(content) {
			return "", fmt.Errorf("%w: the content is not JSON", ErrInvalidAttachment)
		}
	default:
		// Text types share one sniff: http.DetectContentType cannot tell
		// Markdown from CSV from a plain note, and it calls a Markdown file
		// that opens with an HTML comment text/html. What it can tell is
		// that the content is not text at all, which is the mismatch worth
		// rejecting.
		if binaryMediaType(detected) {
			return "", fmt.Errorf("%w: the content is %q, not %q", ErrInvalidAttachment, detected, media)
		}
		if !textualContent(content) {
			return "", fmt.Errorf("%w: the content is not UTF-8 text", ErrInvalidAttachment)
		}
	}
	return media, nil
}

// binaryMediaType reports whether a sniffed media type names binary content.
// application/json is the one application type that is text.
func binaryMediaType(detected string) bool {
	if detected == "application/json" {
		return false
	}
	for _, prefix := range []string{"image/", "audio/", "video/", "font/", "application/"} {
		if strings.HasPrefix(detected, prefix) {
			return true
		}
	}
	return false
}

// textualContent reports whether content is UTF-8 text: no invalid
// sequences and no C0 control bytes other than tab, newline and return.
func textualContent(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, b := range content {
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

// SanitizeAttachmentName reduces an uploaded file name to a bare name: the
// directory part of a POSIX or Windows path is dropped, control characters
// are removed, and the result must be a usable name of at most
// MaxAttachmentNameBytes.
func SanitizeAttachmentName(name string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if index := strings.LastIndexAny(cleaned, `/\`); index >= 0 {
		cleaned = cleaned[index+1:]
	}
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("%w: a file name is required", ErrInvalidAttachment)
	}
	if len(cleaned) > MaxAttachmentNameBytes {
		return "", fmt.Errorf("%w: the file name must be at most %d bytes", ErrInvalidAttachment, MaxAttachmentNameBytes)
	}
	return cleaned, nil
}

// ValidateAttachmentBytes enforces the per-file size limit. An empty file is
// refused: there is nothing to hand the provider.
func ValidateAttachmentBytes(size int64) error {
	if size <= 0 {
		return fmt.Errorf("%w: the file is empty", ErrInvalidAttachment)
	}
	if size > MaxAttachmentBytes {
		return fmt.Errorf("%w: a file must be at most %d bytes", ErrInvalidAttachment, MaxAttachmentBytes)
	}
	return nil
}

// ValidateAttachmentSelection enforces the shape of the attachment list a
// message command carries: well-formed identifiers, no repeats and at most
// MaxMessageAttachments of them. Ownership, the unsent state and the total
// size are checked by the hub, which is the only side that knows them.
func ValidateAttachmentSelection(ids []string) error {
	if len(ids) > MaxMessageAttachments {
		return fmt.Errorf("%w: a message carries at most %d attachments", ErrInvalidAttachment, MaxMessageAttachments)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := ValidateAttachmentID(id); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidAttachment, err)
		}
		if seen[id] {
			return fmt.Errorf("%w: attachment %s is listed twice", ErrInvalidAttachment, id)
		}
		seen[id] = true
	}
	return nil
}

// ValidateAttachmentTotal enforces the per-message total size limit.
func ValidateAttachmentTotal(total int64) error {
	if total > MaxAttachmentBytes {
		return fmt.Errorf("%w: the attachments of one message must be at most %d bytes in total", ErrInvalidAttachment, MaxAttachmentBytes)
	}
	return nil
}

// Attachment is one bound attachment as a message resource and a worker
// control carry it. URL is the hub path that streams the blob.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// AttachmentUpload is the 201 body of an upload. ExpiresAt is when an unsent
// upload is swept; it is null once the attachment is bound to a message.
type AttachmentUpload struct {
	Attachment
	ExpiresAt *time.Time `json:"expires_at"`
}

// Image reports whether the attachment is provider image input rather than a
// text data block.
func (a Attachment) Image() bool { return strings.HasPrefix(a.MIME, "image/") }
