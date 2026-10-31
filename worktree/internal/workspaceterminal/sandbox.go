package workspaceterminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

var ErrSandboxIsolation = errors.New("workspaceterminal: worktree sandbox isolation is unavailable")

var measuredIsolation = sync.OnceValue(func() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ProbeSandbox(ctx); err != nil {
		return workspacesession.IsolationUser
	}
	return workspacesession.IsolationSandbox
})

func AvailableIsolation() string { return measuredIsolation() }

func ProbeSandbox(ctx context.Context) (result error) {
	if !sandboxSupported {
		return ErrSandboxIsolation
	}
	var scratch string
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value := os.Getenv(key); value != "" {
			scratch = value
			break
		}
	}
	if scratch == "" {
		return fmt.Errorf("%w: runner scratch is not provided", ErrSandboxIsolation)
	}
	base, err := os.MkdirTemp(scratch, "terminal-isolation-")
	if err != nil {
		return fmt.Errorf("%w: prepare probe: %w", ErrSandboxIsolation, err)
	}
	// #nosec G703 -- base is the fresh directory returned by MkdirTemp, not a request path.
	defer func() { result = errors.Join(result, os.RemoveAll(base)) }()
	root := filepath.Join(base, "worktree")
	// #nosec G703 -- the fixed worktree child is inside the fresh probe directory.
	if err := os.Mkdir(root, 0o700); err != nil {
		return err
	}
	outside := filepath.Join(base, "outside")
	// #nosec G703 -- the fixed outside child is inside the fresh probe directory.
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		return err
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		return err
	}
	service, err := newService(ctx, root, "/bin/sh", workspacesession.IsolationSandbox, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return err
	}
	terminal, err := service.Open(ctx, workspacesession.TerminalOpen{}, func(workspacesession.TerminalOutput) error { return nil })
	if err != nil {
		return err
	}
	defer terminal.Close()
	script := `stty size >/dev/null && printf allowed > allowed && /bin/sh -c 'test "$(cat allowed)" = allowed && ! cat ../outside && ! cat escape && ! (printf forbidden > ../outside) && ! ln ../outside linked' && printf confirmed > confirmed; exit` + "\n"
	if err := terminal.Write([]byte(script)); err != nil {
		return err
	}
	select {
	case <-terminal.Done():
	case <-ctx.Done():
		return fmt.Errorf("%w: probe did not finish", ErrSandboxIsolation)
	}
	// #nosec G703 -- reads the fixed marker inside the fresh probe worktree.
	confirmed, err := os.ReadFile(filepath.Join(root, "confirmed"))
	if err != nil || string(confirmed) != "confirmed" {
		return fmt.Errorf("%w: enforcement was not confirmed", ErrSandboxIsolation)
	}
	// #nosec G703 -- reads the fixed sibling created by this probe, not a caller path.
	private, err := os.ReadFile(outside)
	if err != nil || string(private) != "private" {
		return fmt.Errorf("%w: probe escaped the worktree", ErrSandboxIsolation)
	}
	return nil
}

func sandboxEnvironment(root string, cols, rows int) []string {
	return []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + root,
		"TMPDIR=" + root,
		"DETENT_WORKSPACE=" + root,
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", rows),
		"LANG=C",
	}
}

func containsPath(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
