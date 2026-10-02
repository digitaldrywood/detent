package runner

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
)

type ValidationEvidence struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Content     []byte `json:"content"`
}

type ValidationEvidenceExecution interface {
	PublishValidationEvidence(context.Context, []ValidationEvidence) error
}

func validationScreenshots(directory string) (evidence []ValidationEvidence, resultErr error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	err = fs.WalkDir(root.FS(), ".detent/validation", func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == ".detent/validation" {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() || entry.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		default:
			return nil
		}
		if len(evidence) == 10 {
			return errors.New("validation evidence exceeds ten screenshots")
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		content, readErr := io.ReadAll(io.LimitReader(file, attachment.MaxBytes+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		if len(content) > attachment.MaxBytes {
			return attachment.ErrTooLarge
		}
		evidence = append(evidence, ValidationEvidence{Name: filepath.Base(path), ContentType: mime.TypeByExtension(filepath.Ext(path)), Content: content})
		return nil
	})
	return evidence, err
}
