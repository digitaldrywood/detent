package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ValidationEvidence struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"content"`
}

type ValidationEvidenceExecution interface {
	PublishValidationEvidence(context.Context, []ValidationEvidence) error
}

func validationScreenshots(directory string, files []tracker.AttemptDiffFile) (evidence []ValidationEvidence, resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	for _, changed := range files {
		name := changed.Path
		if !strings.HasPrefix(name, ".detent/validation/") || changed.Status == "deleted" {
			continue
		}
		if path.Clean(name) != name {
			return nil, fs.ErrInvalid
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		default:
			continue
		}
		entry, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !entry.Mode().IsRegular() {
			continue
		}
		if len(evidence) == 10 {
			return nil, errors.New("validation evidence exceeds ten screenshots")
		}
		file, err := root.Open(name)
		if err != nil {
			return nil, err
		}
		content, readErr := io.ReadAll(io.LimitReader(file, attachment.MaxBytes+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		if len(content) > attachment.MaxBytes {
			return nil, attachment.ErrTooLarge
		}
		evidence = append(evidence, ValidationEvidence{Name: filepath.Base(name), ContentType: mime.TypeByExtension(filepath.Ext(name)), Content: content})
	}
	return evidence, nil
}
