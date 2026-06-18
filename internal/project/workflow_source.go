package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
)

var (
	errMissingWorkflowRefRoot = errors.New("workflow_ref requires project workdir")
	errUnsafeWorkflowPath     = errors.New("workflow path must stay inside the source root")
)

type workflowGitRefSource struct {
	sourceRoot string
	ref        string
	path       string
}

type gitRefWorkflowWatcher struct {
	source   workflowGitRefSource
	interval time.Duration
	logger   *slog.Logger
}

func LoadWorkflow(cfg globalconfig.Project) (workflowconfig.Workflow, error) {
	return LoadWorkflowContext(context.Background(), cfg)
}

func LoadWorkflowContext(ctx context.Context, cfg globalconfig.Project) (workflowconfig.Workflow, error) {
	if strings.TrimSpace(cfg.WorkflowRef) == "" {
		return workflowconfig.LoadWorkflow(cfg.Workflow)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	source, err := newWorkflowGitRefSource(cfg)
	if err != nil {
		return workflowconfig.Workflow{}, err
	}
	workflow, _, err := source.load(ctx)
	return workflow, err
}

func newWorkflowGitRefSource(cfg globalconfig.Project) (workflowGitRefSource, error) {
	sourceRoot := strings.TrimSpace(cfg.Workdir)
	if sourceRoot == "" {
		return workflowGitRefSource{}, errMissingWorkflowRefRoot
	}

	workflowPath, err := workflowRefPath(sourceRoot, cfg.Workflow)
	if err != nil {
		return workflowGitRefSource{}, err
	}

	ref := strings.TrimSpace(cfg.WorkflowRef)
	if ref == "" {
		return workflowGitRefSource{}, errors.New("workflow_ref must not be blank")
	}
	if strings.ContainsAny(ref, "\r\n") {
		return workflowGitRefSource{}, errors.New("workflow_ref must be a single line")
	}

	return workflowGitRefSource{
		sourceRoot: sourceRoot,
		ref:        ref,
		path:       workflowPath,
	}, nil
}

func workflowRefPath(sourceRoot string, workflowPath string) (string, error) {
	workflowPath = strings.TrimSpace(workflowPath)
	if workflowPath == "" {
		return "", errors.New("workflow path must not be blank")
	}

	if filepath.IsAbs(workflowPath) {
		sourceRoot, err := filepath.Abs(sourceRoot)
		if err != nil {
			return "", fmt.Errorf("resolve source root: %w", err)
		}
		workflowPath, err = filepath.Abs(workflowPath)
		if err != nil {
			return "", fmt.Errorf("resolve workflow path: %w", err)
		}
		rel, err := filepath.Rel(sourceRoot, workflowPath)
		if err != nil {
			return "", fmt.Errorf("relativize workflow path: %w", err)
		}
		workflowPath = rel
	}

	workflowPath = filepath.Clean(workflowPath)
	if workflowPath == "." || workflowPath == ".." || strings.HasPrefix(workflowPath, ".."+string(filepath.Separator)) {
		return "", errUnsafeWorkflowPath
	}
	return filepath.ToSlash(workflowPath), nil
}

func (s workflowGitRefSource) load(ctx context.Context) (workflowconfig.Workflow, string, error) {
	revision, err := s.revision(ctx)
	if err != nil {
		return workflowconfig.Workflow{}, "", err
	}

	raw, err := runWorkflowGit(ctx, s.sourceRoot, "show", revision+":"+s.path)
	if err != nil {
		return workflowconfig.Workflow{}, revision, fmt.Errorf("load workflow from %s: %w", s.displayPath(), err)
	}
	workflow, err := workflowconfig.ParseWorkflow(raw)
	if err != nil {
		return workflowconfig.Workflow{}, revision, err
	}
	return workflow, revision, nil
}

func (s workflowGitRefSource) revision(ctx context.Context) (string, error) {
	output, err := runWorkflowGit(ctx, s.sourceRoot, "rev-parse", "--verify", s.ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve workflow ref %s: %w", s.ref, err)
	}
	revision := strings.TrimSpace(string(output))
	if revision == "" {
		return "", fmt.Errorf("resolve workflow ref %s: empty revision", s.ref)
	}
	return revision, nil
}

func (s workflowGitRefSource) displayPath() string {
	return s.ref + ":" + s.path
}

func runWorkflowGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- workflow refs and paths are operator config and are passed as git arguments, not shell.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git -C %s %s: %w\n%s", dir, strings.Join(args, " "), err, output)
	}
	return output, nil
}

func newGitRefWorkflowWatcher(cfg globalconfig.Project, interval time.Duration, logger *slog.Logger) (*gitRefWorkflowWatcher, error) {
	source, err := newWorkflowGitRefSource(cfg)
	if err != nil {
		return nil, err
	}
	if interval <= 0 {
		interval = time.Duration(workflowconfig.DefaultPollingIntervalMS) * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &gitRefWorkflowWatcher{
		source:   source,
		interval: interval,
		logger:   logger,
	}, nil
}

func (w *gitRefWorkflowWatcher) Watch(ctx context.Context) (<-chan configwatcher.Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	updates := make(chan configwatcher.Update, 1)
	go w.run(ctx, updates)
	return updates, nil
}

func (w *gitRefWorkflowWatcher) run(ctx context.Context, updates chan<- configwatcher.Update) {
	defer close(updates)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	lastRevision, lastErr := w.seed(ctx, updates)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastRevision, lastErr = w.reload(ctx, updates, lastRevision, lastErr)
		}
	}
}

func (w *gitRefWorkflowWatcher) seed(ctx context.Context, updates chan<- configwatcher.Update) (string, string) {
	_, revision, err := w.source.load(ctx)
	if err != nil {
		message := err.Error()
		w.send(ctx, updates, configwatcher.Update{Path: w.source.displayPath(), Err: err, At: time.Now()})
		return "", message
	}
	return revision, ""
}

func (w *gitRefWorkflowWatcher) reload(
	ctx context.Context,
	updates chan<- configwatcher.Update,
	lastRevision string,
	lastErr string,
) (string, string) {
	workflow, revision, err := w.source.load(ctx)
	if err != nil {
		message := err.Error()
		if message != lastErr {
			w.send(ctx, updates, configwatcher.Update{Path: w.source.displayPath(), Err: err, At: time.Now()})
		}
		return lastRevision, message
	}
	if revision == lastRevision {
		return lastRevision, ""
	}
	w.send(ctx, updates, configwatcher.Update{
		Path:     w.source.displayPath(),
		Workflow: workflow,
		At:       time.Now(),
	})
	return revision, ""
}

func (w *gitRefWorkflowWatcher) send(ctx context.Context, updates chan<- configwatcher.Update, update configwatcher.Update) {
	select {
	case updates <- update:
	case <-ctx.Done():
	}
}
