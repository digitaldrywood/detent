package citrigger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
)

const lockRetry = 250 * time.Millisecond

type Lock interface {
	Close() error
}

type Dependencies struct {
	Now          func() time.Time
	Wait         func(context.Context, time.Duration) error
	Acquire      func(string) (Lock, error)
	UserCacheDir func() (string, error)
}

type Options struct {
	CoordinationDir string
	Repository      string
	Stagger         time.Duration
}

func Reapply(ctx context.Context, options Options, deps Dependencies, action func(context.Context) error) (err error) {
	options.Repository = strings.TrimSpace(options.Repository)
	if options.Repository == "" {
		return errors.New("coordinate CI trigger label: repository is required")
	}
	if options.Stagger <= 0 {
		return errors.New("coordinate CI trigger label: stagger must be greater than zero")
	}
	if action == nil {
		return errors.New("coordinate CI trigger label: action is required")
	}
	deps = deps.withDefaults()
	dir, err := coordinationDir(options.CoordinationDir, deps.UserCacheDir)
	if err != nil {
		return err
	}
	lockPath, timestampPath := CoordinationPaths(dir, options.Repository)
	lock, err := acquire(ctx, lockPath, deps)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lock.Close())
	}()

	if err := enforceStagger(ctx, timestampPath, options.Stagger, deps); err != nil {
		return err
	}
	if err := action(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(timestampPath, []byte(deps.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("record CI trigger-label timestamp: %w", err)
	}
	return nil
}

func CoordinationPaths(dir string, repository string) (string, string) {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(repository))))
	key := hex.EncodeToString(sum[:8])
	base := filepath.Join(dir, key)
	return base + ".lock", base + ".last"
}

func (deps Dependencies) withDefaults() Dependencies {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Wait == nil {
		deps.Wait = wait
	}
	if deps.Acquire == nil {
		deps.Acquire = func(path string) (Lock, error) { return instancelock.Acquire(path) }
	}
	if deps.UserCacheDir == nil {
		deps.UserCacheDir = os.UserCacheDir
	}
	return deps
}

func coordinationDir(configured string, userCacheDir func() (string, error)) (string, error) {
	if configured = strings.TrimSpace(configured); configured != "" {
		return filepath.Abs(configured)
	}
	root, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	return filepath.Join(root, "detent", "ci-trigger-label"), nil
}

func acquire(ctx context.Context, path string, deps Dependencies) (Lock, error) {
	for {
		lock, err := deps.Acquire(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, instancelock.ErrHeld) {
			return nil, fmt.Errorf("acquire CI trigger-label coordination lock: %w", err)
		}
		if err := deps.Wait(ctx, lockRetry); err != nil {
			return nil, err
		}
	}
}

func enforceStagger(ctx context.Context, timestampPath string, stagger time.Duration, deps Dependencies) error {
	raw, err := os.ReadFile(timestampPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read CI trigger-label timestamp: %w", err)
	}
	last, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return fmt.Errorf("parse CI trigger-label timestamp: %w", err)
	}
	remaining := last.Add(stagger).Sub(deps.Now())
	if remaining <= 0 {
		return nil
	}
	return deps.Wait(ctx, remaining)
}

func wait(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
