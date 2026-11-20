package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
)

var codexHomeAssignmentPattern = regexp.MustCompile(`(?:^|[[:space:];])CODEX_HOME=(?:"([^"]*)"|'([^']*)'|([^[:space:];|&]+))`)

const (
	backendCredentialWatchRetryDelay     = 5 * time.Second
	backendCredentialWatchReconcileDelay = 150 * time.Millisecond
)

type credentialFileStamp struct {
	modified int64
	size     int64
	mode     uint32
}

type backendCredentialWatchTimer interface {
	C() <-chan time.Time
	Reset(time.Duration) bool
	Stop() bool
}

type standardBackendCredentialWatchTimer struct {
	timer *time.Timer
}

func (t *standardBackendCredentialWatchTimer) C() <-chan time.Time {
	return t.timer.C
}

func (t *standardBackendCredentialWatchTimer) Reset(delay time.Duration) bool {
	return t.timer.Reset(delay)
}

func (t *standardBackendCredentialWatchTimer) Stop() bool {
	return t.timer.Stop()
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

			if retrying {
				logger.Info("backend credential file watcher attached", "path", path)
			}
			loggedWatchFailure = false

			if !monitorBackendCredentialFile(
				ctx,
				path,
				updates,
				notify,
				logger,
				retryDelay,
				backendCredentialWatchReconcileDelay,
				&lastStamp,
				func(delay time.Duration) backendCredentialWatchTimer {
					return &standardBackendCredentialWatchTimer{timer: time.NewTimer(delay)}
				},
			) {
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
	reconcileDelay time.Duration,
	lastStamp **credentialFileStamp,
	newTimer func(time.Duration) backendCredentialWatchTimer,
) bool {
	if reconcileDelay <= 0 {
		reconcileDelay = backendCredentialWatchReconcileDelay
	}
	if newTimer == nil {
		newTimer = func(delay time.Duration) backendCredentialWatchTimer {
			return &standardBackendCredentialWatchTimer{timer: time.NewTimer(delay)}
		}
	}
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()
	reconcile := newTimer(reconcileDelay)
	if !reconcile.Stop() {
		<-reconcile.C()
	}
	defer reconcile.Stop()
	var reconcileC <-chan time.Time
	scheduleReconcile := func(reset bool) {
		if reconcileC != nil {
			if !reset {
				return
			}
			if !reconcile.Stop() {
				select {
				case <-reconcile.C():
				default:
				}
			}
		}
		reconcile.Reset(reconcileDelay)
		reconcileC = reconcile.C()
	}
	scheduleReconcile(true)
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
			scheduleReconcile(true)
		case <-poll.C:
			scheduleReconcile(false)
		case <-reconcileC:
			reconcileC = nil
			reconcileBackendCredentialStamp(ctx, path, notify, logger, lastStamp)
		}
	}
}

func reconcileBackendCredentialStamp(
	ctx context.Context,
	path string,
	notify func(context.Context),
	logger *slog.Logger,
	lastStamp **credentialFileStamp,
) {
	stamp, err := readCredentialFileStamp(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("read backend credential file metadata failed", "path", path, "error", err)
		}
		return
	}
	if backendCredentialStampChanged(lastStamp, stamp) && notify != nil {
		notify(ctx)
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
