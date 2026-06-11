package watcher

import (
	"context"
	"errors"
	"log/slog"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
	file *FileWatcher[workflowconfig.Workflow]
}

type options struct {
	debounce time.Duration
	loader   Loader
	logger   *slog.Logger
}

func New(path string, opts ...Option) (*Watcher, error) {
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

	file, err := NewFile(path, func(path string) (workflowconfig.Workflow, error) {
		workflow, err := cfg.loader(path)
		if err == nil {
			err = workflow.Config.Validate()
		}
		return workflow, err
	}, WithFileDebounce(cfg.debounce), WithFileLogger(cfg.logger))
	if err != nil {
		return nil, err
	}

	return &Watcher{file: file}, nil
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

	fileUpdates, err := w.file.Watch(ctx)
	if err != nil {
		return nil, err
	}

	updates := make(chan Update, 1)
	go func() {
		defer close(updates)
		for update := range fileUpdates {
			mapped := Update{
				Path:     update.Path,
				Workflow: update.Value,
				Err:      update.Err,
				At:       update.At,
			}
			select {
			case updates <- mapped:
			case <-ctx.Done():
				return
			}
		}
	}()
	return updates, nil
}

func retryInterval(debounce time.Duration) time.Duration {
	if debounce < reloadRetryInterval {
		return debounce
	}
	return reloadRetryInterval
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
