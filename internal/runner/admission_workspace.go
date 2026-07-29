package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace"
)

type admissionWorkspace struct {
	logger *slog.Logger
	path   string
}

func (w *admissionWorkspace) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	if err := ctx.Err(); err != nil {
		return workspace.Info{}, err
	}
	path, err := os.MkdirTemp("", "detent-admission-")
	if err != nil {
		return workspace.Info{}, err
	}
	w.path = path
	return workspace.Info{
		Path:    path,
		Key:     strings.TrimSpace(issue.ID),
		Created: true,
	}, nil
}

func (w *admissionWorkspace) Cleanup(_ context.Context, path string) error {
	if w.path == "" || filepath.Clean(path) != filepath.Clean(w.path) {
		return errors.New("backlog admission workspace cleanup path does not match the created workspace")
	}
	if err := os.RemoveAll(w.path); err != nil {
		return err
	}
	w.path = ""
	return nil
}

func (*admissionWorkspace) BeforeRun(context.Context, workspace.Info, workspace.Issue) error {
	return nil
}

func (w *admissionWorkspace) AfterRun(ctx context.Context, info workspace.Info, _ workspace.Issue) {
	if err := w.Cleanup(ctx, info.Path); err != nil && w.logger != nil {
		w.logger.Warn("remove backlog admission workspace failed", "workspace_path", info.Path, "error", err)
	}
}

func (*admissionWorkspace) DiffStat(context.Context, workspace.Info, workspace.Issue) (workspace.DiffStat, error) {
	return workspace.DiffStat{}, nil
}
