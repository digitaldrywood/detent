package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	workflowconfig "github.com/digitaldrywood/symphony/internal/config"
)

const (
	defaultDebounce     = 150 * time.Millisecond
	reloadRetryInterval = 5 * time.Millisecond
)

var ErrMissingPath = errors.New("workflow watch path is required")

type Loader func(string) (workflowconfig.Workflow, error)

type Update struct {
	Path     string
	Workflow workflowconfig.Workflow
	Err      error
	At       time.Time
}

type Option func(*options)

type Watcher struct {
	path     string
	dir      string
	debounce time.Duration
	loader   Loader
	logger   *slog.Logger
}

type options struct {
	debounce time.Duration
	loader   Loader
	logger   *slog.Logger
}

func New(path string, opts ...Option) (*Watcher, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrMissingPath
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve workflow watch path: %w", err)
	}

	cfg := options{
		debounce: defaultDebounce,
		loader:   workflowconfig.LoadWorkflow,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.debounce <= 0 {
		cfg.debounce = defaultDebounce
	}
	if cfg.loader == nil {
		cfg.loader = workflowconfig.LoadWorkflow
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	return &Watcher{
		path:     filepath.Clean(absolute),
		dir:      filepath.Dir(absolute),
		debounce: cfg.debounce,
		loader:   cfg.loader,
		logger:   cfg.logger,
	}, nil
}

func WithDebounce(debounce time.Duration) Option {
	return func(opts *options) {
		opts.debounce = debounce
	}
}

func WithLoader(loader Loader) Option {
	return func(opts *options) {
		opts.loader = loader
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(opts *options) {
		opts.logger = logger
	}
}

func (w *Watcher) Watch(ctx context.Context) (<-chan Update, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create workflow watcher: %w", err)
	}
	if err := fsWatcher.Add(w.dir); err != nil {
		closeErr := fsWatcher.Close()
		return nil, errors.Join(fmt.Errorf("watch workflow directory: %w", err), closeErr)
	}

	updates := make(chan Update, 1)
	go w.run(ctx, fsWatcher, updates)
	return updates, nil
}

func (w *Watcher) run(ctx context.Context, fsWatcher *fsnotify.Watcher, updates chan<- Update) {
	defer close(updates)
	defer func() {
		if err := fsWatcher.Close(); err != nil {
			w.logger.Warn("close workflow watcher failed", "path", w.path, "error", err)
		}
	}()

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fsWatcher.Events:
			if !ok {
				return
			}
			if w.matches(event) {
				resetTimer(timer, w.debounce)
				timerC = timer.C
			}
		case err, ok := <-fsWatcher.Errors:
			if !ok {
				return
			}
			w.send(ctx, updates, Update{Path: w.path, Err: err, At: time.Now()})
		case <-timerC:
			timerC = nil
			w.reload(ctx, updates)
		}
	}
}

func (w *Watcher) matches(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	return filepath.Clean(event.Name) == w.path
}

func (w *Watcher) reload(ctx context.Context, updates chan<- Update) {
	update := Update{
		Path: w.path,
		At:   time.Now(),
	}

	workflow, err := w.load(ctx)
	if err != nil {
		update.Err = err
	} else {
		update.Workflow = workflow
	}

	w.send(ctx, updates, update)
}

func (w *Watcher) load(ctx context.Context) (workflowconfig.Workflow, error) {
	workflow, err := w.loadOnce()
	if err == nil {
		return workflow, nil
	}
	lastErr := err

	deadline := time.NewTimer(w.debounce)
	defer deadline.Stop()
	retry := time.NewTicker(retryInterval(w.debounce))
	defer retry.Stop()

	for {
		select {
		case <-ctx.Done():
			return workflowconfig.Workflow{}, ctx.Err()
		case <-deadline.C:
			return workflowconfig.Workflow{}, lastErr
		case <-retry.C:
			workflow, err := w.loadOnce()
			if err == nil {
				return workflow, nil
			}
			lastErr = err
		}
	}
}

func (w *Watcher) loadOnce() (workflowconfig.Workflow, error) {
	workflow, err := w.loader(w.path)
	if err == nil {
		err = workflow.Config.Validate()
	}
	return workflow, err
}

func retryInterval(debounce time.Duration) time.Duration {
	if debounce < reloadRetryInterval {
		return debounce
	}
	return reloadRetryInterval
}

func (w *Watcher) send(ctx context.Context, updates chan<- Update, update Update) {
	select {
	case updates <- update:
	case <-ctx.Done():
	}
}

func resetTimer(timer *time.Timer, debounce time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(debounce)
}
