package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaxBytes = conversation.MaxAttachmentBytes
const MaxPixels = 16000000
const OrphanTTL = conversation.AttachmentTTL

var ErrInvalid = errors.New("invalid attachment")
var ErrTooLarge = errors.New("attachment exceeds 20 MiB")

type Metadata struct {
	Reference           string            `json:"reference,omitempty"`
	ReferencedBy        []SourceReference `json:"referenced_by,omitempty"`
	ID                  string            `json:"id"`
	ProjectID           string            `json:"project_id"`
	Uploader            string            `json:"uploader"`
	Name                string            `json:"name"`
	ContentType         string            `json:"content_type"`
	Size                int64             `json:"size"`
	SHA256              string            `json:"sha256"`
	Width               int               `json:"width"`
	Height              int               `json:"height"`
	CreatedAt           time.Time         `json:"created_at"`
	WorkItemID          string            `json:"work_item_id,omitempty"`
	CommentID           string            `json:"comment_id,omitempty"`
	DeletedAt           *time.Time        `json:"deleted_at,omitempty"`
	AuthorizedPrincipal string            `json:"authorized_principal,omitempty"`
}

type UploadRequest struct {
	Metadata
	RequestID string           `json:"request_id,omitempty"`
	Evidence  *EvidenceRequest `json:"evidence,omitempty"`
}

type EvidenceRequest struct {
	tracker.Mutation
	WorkItemID tracker.NativeWorkItemID `json:"work_item_id"`
	AttemptID  string                   `json:"attempt_id"`
	Caption    string                   `json:"caption"`
}

type SourceReference struct {
	WorkItemID string `json:"work_item_id"`
	CommentID  string `json:"comment_id,omitempty"`
}

func (m Metadata) Validate() error {
	name, err := conversation.SanitizeAttachmentName(m.Name)
	if err != nil || name != m.Name || conversation.ValidateAttachmentID(m.ID) != nil || m.Size <= 0 || m.Size > MaxBytes {
		return ErrInvalid
	}
	digest, err := hex.DecodeString(m.SHA256)
	if err != nil || len(digest) != sha256.Size || m.SHA256 != strings.ToLower(m.SHA256) {
		return ErrInvalid
	}
	switch m.ContentType {
	case "image/png", "image/jpeg":
		if m.Width <= 0 || m.Height <= 0 || m.Width > MaxPixels || m.Height > MaxPixels || int64(m.Width)*int64(m.Height) > MaxPixels {
			return ErrInvalid
		}
	case "application/pdf", "text/plain", "text/markdown", "text/csv", "application/json":
		if m.Width != 0 || m.Height != 0 {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type Upload struct {
	Metadata
	File *os.File
}

func (u *Upload) Close() error {
	return errors.Join(u.File.Close(), os.Remove(u.File.Name()))
}

func Prepare(reader io.Reader, name, declared, scratch string) (upload *Upload, resultErr error) {
	name, err := conversation.SanitizeAttachmentName(name)
	if err != nil {
		return nil, ErrInvalid
	}
	if scratch == "" {
		return nil, errors.New("attachment scratch directory is unavailable")
	}
	file, err := os.CreateTemp(scratch, "detent-attachment-*")
	if err != nil {
		return nil, err
	}
	u := &Upload{File: file, Metadata: Metadata{ID: conversation.NewAttachmentID(), Name: name}}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, u.Close())
		}
	}()
	size, err := io.Copy(file, io.LimitReader(reader, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if size > MaxBytes {
		return nil, ErrTooLarge
	}
	if size == 0 {
		return nil, ErrInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	media, err := Sniff(declared, content)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(media, "image/") {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
			return nil, ErrInvalid
		}
		picture, _, err := image.Decode(bytes.NewReader(content))
		if err != nil {
			return nil, ErrInvalid
		}
		if err := file.Truncate(0); err != nil {
			return nil, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if media == "image/jpeg" {
			err = jpeg.Encode(file, picture, &jpeg.Options{Quality: 90})
		} else {
			media = "image/png"
			err = png.Encode(file, picture)
		}
		if err != nil {
			return nil, err
		}
		u.Width, u.Height = cfg.Width, cfg.Height
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	hash := sha256.New()
	u.Size, err = io.Copy(hash, io.LimitReader(file, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if u.Size > MaxBytes {
		return nil, ErrTooLarge
	}
	u.ContentType, u.SHA256 = media, hex.EncodeToString(hash.Sum(nil))
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return u, nil
}

func Sniff(declared string, content []byte) (string, error) {
	media := conversation.NormalizeMediaType(declared)
	detected := conversation.NormalizeMediaType(http.DetectContentType(content))
	if detected == "application/pdf" && (media == "" || media == "application/pdf") {
		return "application/pdf", nil
	}
	if detected == "text/html" || detected == "text/xml" {
		return "", ErrInvalid
	}
	media, err := conversation.SniffAttachment(declared, content)
	if err != nil {
		return "", ErrInvalid
	}
	if !strings.HasPrefix(media, "image/") {
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf}))
		detected := conversation.NormalizeMediaType(http.DetectContentType(trimmed))
		if detected == "text/html" || detected == "text/xml" || strings.HasPrefix(strings.ToLower(string(trimmed[:min(len(trimmed), 512)])), "<svg") {
			return "", ErrInvalid
		}
	}
	return media, nil
}
