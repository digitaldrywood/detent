package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type FileLoader[T any] func(string) (T, error)

type FileUpdate[T any] struct {
	Path  string
	Value T
	Err   error
	At    time.Time
}

type FileOption func(*fileOptions)

type FileWatcher[T any] struct {
	path     string
	dir      string
	debounce time.Duration
	loader   FileLoader[T]
	logger   *slog.Logger
}

type fileOptions struct {
	debounce time.Duration
	logger   *slog.Logger
}

func NewFile[T any](path string, loader FileLoader[T], opts ...FileOption) (*FileWatcher[T], error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrMissingPath
	}
	if loader == nil {
		return nil, errors.New("config watch loader is required")
	}

	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config watch path: %w", err)
	}

	cfg := fileOptions{
		debounce: defaultDebounce,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.debounce <= 0 {
		cfg.debounce = defaultDebounce
	}
	if cfg.logger == nil {
		cfg.logger = slog.Default()
	}

	return &FileWatcher[T]{
		path:     filepath.Clean(absolute),
		dir:      filepath.Dir(absolute),
		debounce: cfg.debounce,
		loader:   loader,
		logger:   cfg.logger,
	}, nil
}

func WithFileDebounce(debounce time.Duration) FileOption {
	return func(opts *fileOptions) {
		opts.debounce = debounce
	}
}

func WithFileLogger(logger *slog.Logger) FileOption {
	return func(opts *fileOptions) {
		opts.logger = logger
	}
}

func (w *FileWatcher[T]) Watch(ctx context.Context) (<-chan FileUpdate[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create config watcher: %w", err)
	}
	if err := fsWatcher.Add(w.dir); err != nil {
		closeErr := fsWatcher.Close()
		return nil, errors.Join(fmt.Errorf("watch config directory: %w", err), closeErr)
	}

	updates := make(chan FileUpdate[T], 1)
	go w.run(ctx, fsWatcher, updates)
	return updates, nil
}

func (w *FileWatcher[T]) run(ctx context.Context, fsWatcher *fsnotify.Watcher, updates chan<- FileUpdate[T]) {
	defer close(updates)
	defer func() {
		if err := fsWatcher.Close(); err != nil {
			w.logger.Warn("close config watcher failed", "path", w.path, "error", err)
		}
	}()

	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	var lastUpdate *FileUpdate[T]

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
			w.send(ctx, updates, FileUpdate[T]{Path: w.path, Err: err, At: time.Now()})
		case <-timerC:
			timerC = nil
			update := w.reload(ctx)
			if sameFileUpdate(update, lastUpdate) {
				continue
			}
			w.send(ctx, updates, update)
			last := update
			lastUpdate = &last
		}
	}
}

func (w *FileWatcher[T]) matches(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	return filepath.Clean(event.Name) == w.path
}

func (w *FileWatcher[T]) reload(ctx context.Context) FileUpdate[T] {
	update := FileUpdate[T]{
		Path: w.path,
		At:   time.Now(),
	}

	value, err := w.load(ctx)
	if err != nil {
		update.Err = err
		return update
	}
	update.Value = value

	return update
}

func (w *FileWatcher[T]) load(ctx context.Context) (T, error) {
	value, err := w.loader(w.path)
	if err == nil {
		return value, nil
	}
	lastErr := err

	deadline := time.NewTimer(w.debounce)
	defer deadline.Stop()
	retry := time.NewTicker(retryInterval(w.debounce))
	defer retry.Stop()

	for {
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-deadline.C:
			var zero T
			return zero, lastErr
		case <-retry.C:
			value, err := w.loader(w.path)
			if err == nil {
				return value, nil
			}
			lastErr = err
		}
	}
}

func (w *FileWatcher[T]) send(ctx context.Context, updates chan<- FileUpdate[T], update FileUpdate[T]) {
	select {
	case updates <- update:
	case <-ctx.Done():
	}
}

func sameFileUpdate[T any](update FileUpdate[T], last *FileUpdate[T]) bool {
	if last == nil || update.Path != last.Path {
		return false
	}
	if (update.Err == nil) != (last.Err == nil) {
		return false
	}
	if update.Err != nil {
		return update.Err.Error() == last.Err.Error()
	}
	return reflect.DeepEqual(update.Value, last.Value)
}
