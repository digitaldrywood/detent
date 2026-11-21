package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestLocalValidationLaunchUsesWorkerScratchRoot(t *testing.T) {
	temporary := t.TempDir()
	workerTemp := filepath.Join(temporary, "worker")
	controllerTemp := filepath.Join(temporary, "controller")
	for _, dir := range []string{workerTemp, controllerTemp} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	workerAlias := filepath.Join(temporary, "worker-alias")
	if err := os.Symlink(workerTemp, workerAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", workerAlias)

	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{
		ProcessSource: emptyProcessSource, Root: filepath.Join(t.TempDir(), "workspaces"),
		SourceRoot: initRunnerSourceRepo(t), AutoBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	issue := workspace.Issue{
		ID: "wi_example", Identifier: "example-1",
		AttemptWorkspace: workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_0123456789abcdef0123456789abcdef"},
	}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	issue.PullRequestHeadSHA, err = backend.Head(t.Context(), info, issue)
	if err != nil {
		t.Fatal(err)
	}

	sessions := &fakeSessionStore{sessionID: 41}
	callbacks := (&Runner{store: sessions}).LocalRunCallbacks(RunRequest{})
	left, right := net.Pipe()
	var cleanupRoot, cleanupPath string
	controller := NewSSHPeer(t.Context(), left, left, func(ctx context.Context, method string, args []json.RawMessage) (result any, err error) {
		if method != "local.start" {
			return callbacks.Handle(ctx, method, args)
		}
		if err := os.Setenv("TMPDIR", controllerTemp); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, os.Setenv("TMPDIR", workerAlias)) }()
		result, err = callbacks.Handle(ctx, method, args)
		if err == nil && sessions.started.Worker != nil && sessions.started.Worker.CleanupPath != "" {
			cleanupRoot = sessions.started.Worker.CleanupRoot
			cleanupPath = sessions.started.Worker.CleanupPath
		}
		return result, err
	})
	worker := NewSSHPeer(t.Context(), right, right, nil)
	t.Cleanup(func() {
		_ = left.Close()
		_ = right.Close()
		<-controller.Context().Done()
		<-worker.Context().Done()
	})

	store := LocalSessionStore{SSHSessionStore: SSHSessionStore{Peer: worker}, Combined: true}
	ctx := workspace.WithAttemptWorkspace(t.Context(), issue.AttemptWorkspace)
	ctx = withValidationProcessOwner(ctx, store, "prj_example", connector.Issue{ID: issue.ID, Identifier: issue.Identifier}, 1)
	result, err := backend.RunReviewCommand(ctx, info, issue, "printf 'validation ran'")
	if err != nil || result.ExitCode != 0 || result.Output != "validation ran" {
		t.Fatalf("host validation launch: result=%+v error=%v", result, err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(workspace.WorkerScratchRoot(info.Path))
	if err != nil {
		t.Fatal(err)
	}
	if cleanupRoot != canonicalRoot || !strings.HasPrefix(cleanupPath, canonicalRoot+string(filepath.Separator)) {
		t.Fatalf("cleanup registration = %q under %q, want worker scratch under %q", cleanupPath, cleanupRoot, canonicalRoot)
	}
	if _, err := os.Stat(cleanupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validated scratch was not removed: %v", err)
	}
}
