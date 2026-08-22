package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
)

var codexHomeAssignmentPattern = regexp.MustCompile(`(?:^|[[:space:];])CODEX_HOME=(?:"([^"]*)"|'([^']*)'|([^[:space:];|&]+))`)

const backendCredentialWatchRetryDelay = 5 * time.Second

type backendCredentialTarget struct {
	projectID project.ID
	scope     backendcapacity.Scope
}

type backendCredentialWatch struct {
	path    string
	targets []backendCredentialTarget
}

type credentialFileStamp struct {
	modified int64
	size     int64
	mode     uint32
}

type backendCredentialWatchSet struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

func startBackendCredentialWatchers(
	ctx context.Context,
	registry *project.Registry,
	events *hub.Hub[project.Event],
	logger *slog.Logger,
) <-chan struct{} {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	var eventUpdates <-chan project.Event
	var subscription *hub.Subscription[project.Event]
	if events != nil {
		var err error
		subscription, err = events.Subscribe(ctx)
		if err != nil {
			logger.Warn("subscribe backend credential watcher to project events failed", "error", err)
		} else {
			eventUpdates = subscription.C()
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if subscription != nil {
			defer subscription.Close()
		}
		watchers := startBackendCredentialWatchSet(ctx, registry, logger)
		defer watchers.stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-eventUpdates:
				if !ok {
					eventUpdates = nil
					continue
				}
				switch event.Kind {
				case project.EventStarted, project.EventStopped, project.EventWorkflowReloaded:
					watchers.stop()
					watchers = startBackendCredentialWatchSet(ctx, registry, logger)
				}
			}
		}
	}()
	return done
}

func startBackendCredentialWatchSet(ctx context.Context, registry *project.Registry, logger *slog.Logger) backendCredentialWatchSet {
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	watches := configuredBackendCredentialWatches(registry, logger)
	var workers sync.WaitGroup
	for _, watch := range watches {
		watch := watch
		watchDone, err := startBackendCredentialFileWatcher(watchCtx, watch.path, func(ctx context.Context) {
			notifyBackendCredentialTargets(ctx, registry, watch.targets, logger)
		}, logger)
		if err != nil {
			logger.Warn("watch backend credential file failed", "path", watch.path, "error", err)
			continue
		}
		workers.Go(func() {
			<-watchDone
		})
	}
	go func() {
		workers.Wait()
		close(done)
	}()
	return backendCredentialWatchSet{cancel: cancel, done: done}
}

func (w backendCredentialWatchSet) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	if w.done != nil {
		<-w.done
	}
}

func configuredBackendCredentialWatches(registry *project.Registry, logger *slog.Logger) []backendCredentialWatch {
	if registry == nil {
		return nil
	}
	byPath := map[string][]backendCredentialTarget{}
	seen := map[string]struct{}{}
	for _, trackedProject := range registry.List() {
		if trackedProject == nil || trackedProject.Orchestrator() == nil {
			continue
		}
		for _, backend := range trackedProject.Workflow().Config.AgentBackendConfigs() {
			if backend.Kind != workflowconfig.AgentBackendCodex {
				continue
			}
			path, err := codexCredentialPath(backend.Command, os.LookupEnv, os.UserHomeDir)
			if err != nil {
				logger.Warn("resolve codex credential path failed", "project_id", trackedProject.ID(), "backend_id", backend.ID, "error", err)
				continue
			}
			provider := strings.TrimSpace(backend.Provider)
			if provider == "" {
				provider = strings.TrimSpace(backend.CodexOptions().ModelProvider)
			}
			target := backendCredentialTarget{
				projectID: trackedProject.ID(),
				scope: backendcapacity.Scope{
					BackendID:   backend.ID,
					BackendKind: backend.Kind,
					Provider:    provider,
				}.Normalize(),
			}
			key := path + "\x00" + string(target.projectID) + "\x00" + target.scope.Key()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			byPath[path] = append(byPath[path], target)
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	watches := make([]backendCredentialWatch, 0, len(paths))
	for _, path := range paths {
		watches = append(watches, backendCredentialWatch{path: path, targets: byPath[path]})
	}
	return watches
}

func codexCredentialPath(
	command string,
	lookupEnv func(string) (string, bool),
	userHomeDir func() (string, error),
) (string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if userHomeDir == nil {
		userHomeDir = os.UserHomeDir
	}
	root := codexHomeFromCommand(command)
	if root == "" {
		root, _ = lookupEnv("CODEX_HOME")
	}
	root = os.Expand(root, func(name string) string {
		value, _ := lookupEnv(name)
		return value
	})
	root = strings.TrimSpace(root)
	if root == "" {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		root = filepath.Join(home, ".codex")
	} else if root == "~" || strings.HasPrefix(root, "~/") {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand codex home: %w", err)
		}
		root = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(root, "~"), "/"))
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve codex home: %w", err)
	}
	return filepath.Join(filepath.Clean(absolute), "auth.json"), nil
}

func codexHomeFromCommand(command string) string {
	match := codexHomeAssignmentPattern.FindStringSubmatch(command)
	if len(match) == 0 {
		return ""
	}
	for _, value := range match[1:] {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func startBackendCredentialFileWatcher(
	ctx context.Context,
	path string,
	notify func(context.Context),
	logger *slog.Logger,
) (<-chan struct{}, error) {
	return startBackendCredentialFileWatcherWithRetry(ctx, path, notify, logger, backendCredentialWatchRetryDelay)
}

func startBackendCredentialFileWatcherWithRetry(
	ctx context.Context,
	path string,
	notify func(context.Context),
	logger *slog.Logger,
	retryDelay time.Duration,
) (<-chan struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	if retryDelay <= 0 {
		retryDelay = backendCredentialWatchRetryDelay
	}
	watcher, err := configwatcher.NewFile(path, readCredentialFileStamp, configwatcher.WithFileLogger(logger))
	if err != nil {
		return nil, err
	}
	updates, watchErr := watcher.Watch(ctx)
	var initialStamp *credentialFileStamp
	if watchErr == nil {
		stamp, stampErr := readCredentialFileStamp(path)
		if stampErr == nil {
			initialStamp = &stamp
		} else if !errors.Is(stampErr, os.ErrNotExist) {
			logger.Warn("read backend credential file metadata failed", "path", path, "error", stampErr)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lastStamp := initialStamp
		retrying := watchErr != nil
		loggedWatchFailure := false
		for {
			if watchErr != nil {
				if !loggedWatchFailure {
					logger.Warn("watch backend credential file failed; retrying", "path", path, "retry_delay", retryDelay, "error", watchErr)
				}
				loggedWatchFailure = true
				retrying = true
				if !waitForBackendCredentialWatchRetry(ctx, retryDelay) {
					return
				}
				updates, watchErr = watcher.Watch(ctx)
				continue
			}

			if stamp, stampErr := readCredentialFileStamp(path); stampErr == nil {
				if backendCredentialStampChanged(&lastStamp, stamp) && notify != nil {
					notify(ctx)
				}
			} else if !errors.Is(stampErr, os.ErrNotExist) {
				logger.Warn("read backend credential file metadata failed", "path", path, "error", stampErr)
			}
			if retrying {
				logger.Info("backend credential file watcher attached", "path", path)
			}
			loggedWatchFailure = false

			if !monitorBackendCredentialFile(ctx, path, updates, notify, logger, retryDelay, &lastStamp) {
				return
			}
			retrying = true
			if !waitForBackendCredentialWatchRetry(ctx, retryDelay) {
				return
			}
			updates, watchErr = watcher.Watch(ctx)
		}
	}()
	return done, nil
}

func monitorBackendCredentialFile(
	ctx context.Context,
	path string,
	updates <-chan configwatcher.FileUpdate[credentialFileStamp],
	notify func(context.Context),
	logger *slog.Logger,
	pollInterval time.Duration,
	lastStamp **credentialFileStamp,
) bool {
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case update, ok := <-updates:
			if !ok {
				return true
			}
			if update.Err != nil {
				logger.Warn("reload backend credential file metadata failed", "path", update.Path, "error", update.Err)
				continue
			}
			if backendCredentialStampChanged(lastStamp, update.Value) && notify != nil {
				notify(ctx)
			}
		case <-poll.C:
			stamp, err := readCredentialFileStamp(path)
			if err == nil && backendCredentialStampChanged(lastStamp, stamp) && notify != nil {
				notify(ctx)
			}
		}
	}
}

func backendCredentialStampChanged(last **credentialFileStamp, current credentialFileStamp) bool {
	if *last != nil && **last == current {
		return false
	}
	stamp := current
	*last = &stamp
	return true
}

func waitForBackendCredentialWatchRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func readCredentialFileStamp(path string) (credentialFileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return credentialFileStamp{}, err
	}
	return credentialFileStamp{
		modified: info.ModTime().UnixNano(),
		size:     info.Size(),
		mode:     uint32(info.Mode()),
	}, nil
}

func notifyBackendCredentialTargets(ctx context.Context, registry *project.Registry, targets []backendCredentialTarget, logger *slog.Logger) {
	for _, target := range targets {
		trackedProject, ok := registry.Get(target.projectID)
		if !ok || trackedProject == nil || trackedProject.Orchestrator() == nil {
			continue
		}
		scheduled, err := trackedProject.Orchestrator().BackendCredentialChanged(ctx, target.scope)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, orchestrator.ErrStopped) {
				logger.Warn("notify backend credential change failed", "project_id", target.projectID, "backend_id", target.scope.BackendID, "error", err)
			}
			continue
		}
		logger.Info("backend credential file changed", "project_id", target.projectID, "backend_id", target.scope.BackendID, "capacity_probe_scheduled", scheduled)
	}
}
