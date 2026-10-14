package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/store"
)

type RunnerSetup struct {
	runnerID string
	store    store.ProjectRunnerSetupStore
	logger   *slog.Logger
	mu       sync.Mutex
	projects map[string]*sync.Mutex
}

func NewRunnerSetup(runnerID string, localStore store.ProjectRunnerSetupStore, logger *slog.Logger) *RunnerSetup {
	if logger == nil {
		logger = slog.Default()
	}
	return &RunnerSetup{runnerID: runnerID, store: localStore, logger: logger, projects: make(map[string]*sync.Mutex)}
}

func (s *RunnerSetup) Prepare(ctx context.Context, cfg globalconfig.Project) (err error) {
	s.mu.Lock()
	lock := s.projects[cfg.ID]
	if lock == nil {
		lock = &sync.Mutex{}
		s.projects[cfg.ID] = lock
	}
	s.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	defer func() {
		if err != nil {
			s.logger.Error("runner project setup failed", "runner_id", s.runnerID, "project_id", cfg.ID, "attribution", "instance", "error", err)
		}
	}()
	workflow, err := LoadWorkflowContext(ctx, cfg)
	if errors.Is(err, workflowconfig.ErrNoProjectDefinition) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load runner project setup: %w", err)
	}
	hooks := workflow.Config.Hooks
	if hooks.RunnerSetup == "" {
		return nil
	}
	if s.store == nil || s.runnerID == "" {
		return errors.New("runner project setup requires a runner identity and local store")
	}
	content, err := runnerSetupContent(ctx, cfg, hooks.RunnerSetup, workflow.Definition.Revision)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(append([]byte(hooks.Shell+"\x00"), content...))
	hash := hex.EncodeToString(digest[:])
	previous, err := s.store.ProjectRunnerSetupHash(ctx, s.runnerID, cfg.ID)
	if err != nil {
		return err
	}
	if previous == hash {
		return nil
	}
	if err := s.store.SetProjectRunnerSetupHash(ctx, s.runnerID, cfg.ID, ""); err != nil {
		return err
	}
	setupCtx, cancel := context.WithTimeout(ctx, time.Duration(hooks.TimeoutMS)*time.Millisecond)
	defer cancel()
	cmd := commandshell.Command(setupCtx, string(content), hooks.Shell)
	procgroup.Configure(setupCtx, cmd)
	cmd.Dir = cfg.Workdir
	cmd.Env = os.Environ()
	cmd.WaitDelay = time.Second
	s.logger.Info("running runner project setup", "runner_id", s.runnerID, "project_id", cfg.ID, "script", hooks.RunnerSetup, "content_hash", hash)
	if err := cmd.Run(); err != nil {
		if setupCtx.Err() != nil {
			err = setupCtx.Err()
		}
		return fmt.Errorf("runner project setup %s: %w", hooks.RunnerSetup, err)
	}
	if err := s.store.SetProjectRunnerSetupHash(ctx, s.runnerID, cfg.ID, hash); err != nil {
		return err
	}
	s.logger.Info("runner project setup completed", "runner_id", s.runnerID, "project_id", cfg.ID, "content_hash", hash)
	return nil
}

func runnerSetupContent(ctx context.Context, cfg globalconfig.Project, script, revision string) ([]byte, error) {
	if !filepath.IsLocal(script) {
		return nil, errors.New("runner project setup requires a repository-relative file path")
	}
	if cfg.WorkflowRef != "" {
		content, err := runWorkflowGit(ctx, cfg.Workdir, "show", revision+":"+filepath.ToSlash(script))
		if err != nil {
			return nil, fmt.Errorf("read runner project setup from workflow ref: %w", err)
		}
		return content, nil
	}
	root, err := filepath.EvalSymlinks(cfg.Workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve runner project checkout: %w", err)
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, script))
	if err != nil {
		return nil, fmt.Errorf("resolve runner project setup: %w", err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, errors.New("runner project setup must remain inside the repository")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read runner project setup: %w", err)
	}
	return content, nil
}
