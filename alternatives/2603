package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

func workspaceEvidenceSource(directory string) func(context.Context, string) (ValidationEvidence, error) {
	return func(ctx context.Context, path string) (result ValidationEvidence, resultErr error) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if directory == "" || path == "" || filepath.IsAbs(path) || !filepath.IsLocal(path) {
			return result, errors.New("evidence path must be relative to the attempt workspace")
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return result, err
		}
		defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
		info, err := root.Stat(path)
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() {
			return result, attachment.ErrInvalid
		}
		file, err := root.Open(path)
		if err != nil {
			return result, err
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		info, err = file.Stat()
		if err != nil {
			return result, err
		}
		if !info.Mode().IsRegular() {
			return result, attachment.ErrInvalid
		}
		if info.Size() > attachment.MaxBytes {
			return result, attachment.ErrTooLarge
		}
		content, err := io.ReadAll(io.LimitReader(file, attachment.MaxBytes+1))
		if err != nil {
			return result, err
		}
		if len(content) > attachment.MaxBytes {
			return result, attachment.ErrTooLarge
		}
		var declared string
		switch strings.ToLower(filepath.Ext(path)) {
		case ".log", ".txt":
			declared = "text/plain"
		case ".md", ".markdown":
			declared = "text/markdown"
		case ".csv":
			declared = "text/csv"
		case ".json":
			declared = "application/json"
		case ".png":
			declared = "image/png"
		case ".jpg", ".jpeg":
			declared = "image/jpeg"
		case ".gif":
			declared = "image/gif"
		case ".webp":
			declared = "image/webp"
		}
		media, err := attachment.Sniff(declared, content)
		if err != nil || !conversation.AttachmentMediaAllowed(media) {
			return result, attachment.ErrInvalid
		}
		return ValidationEvidence{Name: filepath.Base(path), ContentType: media, Content: content}, ctx.Err()
	}
}
