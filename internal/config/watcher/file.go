package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type FileLoader[T any] func(string) (T, error)

type FileUpdate[T any] struct {
	Path       string
	Value      T
	Err        error
	WatcherErr bool
	At         time.Time
}

type FileOption func(*fileOptions)

type FileWatcher[T any] struct {
	path     string
	files    []watchedFile
	dirs     []string
	debounce time.Duration
	loader   FileLoader[T]
	logger   *slog.Logger
}

type fileOptions struct {
	debounce   time.Duration
	logger     *slog.Logger
	watchPaths []string
}

type watchedFile struct {
	path   string
	target string
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

	path = filepath.Clean(absolute)
	files := []watchedFile{{path: path, target: resolveWatchPath(path)}}
	seen := map[string]struct{}{path: {}}
	for _, additionalPath := range cfg.watchPaths {
		additionalPath = strings.TrimSpace(additionalPath)
		if additionalPath == "" {
			continue
		}
		additionalAbsolute, err := filepath.Abs(additionalPath)
		if err != nil {
			return nil, fmt.Errorf("resolve additional config watch path: %w", err)
		}
		additionalPath = filepath.Clean(additionalAbsolute)
		if _, ok := seen[additionalPath]; ok {
			continue
		}
		seen[additionalPath] = struct{}{}
		files = append(files, watchedFile{path: additionalPath, target: resolveWatchPath(additionalPath)})
	}
	watchPaths := make([]string, 0, len(files)*2)
	for _, file := range files {
		watchPaths = append(watchPaths, file.path, file.target)
	}

	return &FileWatcher[T]{
		path:     path,
		files:    files,
		dirs:     watchDirs(watchPaths...),
		debounce: cfg.debounce,
		loader:   loader,
		logger:   cfg.logger,
	}, nil
}

func watchDirs(paths ...string) []string {
	seen := map[string]struct{}{}
	dirs := make([]string, 0, len(paths))
	for _, path := range paths {
		dir := filepath.Dir(path)
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	return dirs
}

func resolveWatchPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return path
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

func WithFileWatchPaths(paths ...string) FileOption {
	return func(opts *fileOptions) {
		opts.watchPaths = append(opts.watchPaths, paths...)
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
	for _, dir := range w.dirs {
		if err := fsWatcher.Add(dir); err != nil {
			closeErr := fsWatcher.Close()
			return nil, errors.Join(fmt.Errorf("watch config directory %s: %w", dir, err), closeErr)
		}
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
				w.refreshWatchPath(fsWatcher.Add)
				resetTimer(timer, w.debounce)
				timerC = timer.C
			}
		case err, ok := <-fsWatcher.Errors:
			if !ok {
				return
			}
			w.send(ctx, updates, FileUpdate[T]{Path: w.path, Err: err, WatcherErr: true, At: time.Now()})
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
	if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) == 0 {
		return false
	}
	name := filepath.Clean(event.Name)
	for _, file := range w.files {
		if name == file.path || name == file.target {
			return true
		}
	}
	return false
}

func (w *FileWatcher[T]) refreshWatchPath(addDir func(string) error) {
	for index := range w.files {
		watchPath := resolveWatchPath(w.files[index].path)
		if watchPath == w.files[index].target {
			continue
		}

		w.files[index].target = watchPath
		for _, dir := range watchDirs(w.files[index].path, watchPath) {
			if hasWatchDir(w.dirs, dir) {
				continue
			}
			if addDir != nil {
				if err := addDir(dir); err != nil {
					w.logger.Warn("watch config symlink target directory failed",
						"path", w.files[index].path,
						"target", watchPath,
						"dir", dir,
						"error", err,
					)
					continue
				}
			}
			w.dirs = append(w.dirs, dir)
		}
	}
}

func hasWatchDir(dirs []string, dir string) bool {
	return slices.Contains(dirs, dir)
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
