package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type countHostSessionStore struct {
	*fakeSessionStore
	reaps int
}

func (s *countHostSessionStore) MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error {
	s.reaps++
	return nil
}

func TestNativeHostCommandWritesOnlySessionBoundaries(t *testing.T) {
	sessions := &countHostSessionStore{fakeSessionStore: &fakeSessionStore{sessionID: 1}}
	root := t.TempDir()
	issue := connector.Issue{ID: "wi_example", Identifier: "EXAMPLE-1"}
	ctx := workspace.WithAttemptWorkspace(t.Context(), workspace.AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_example"})
	ctx = withValidationProcessOwner(ctx, sessions, "prj_example", issue, 1)
	owner := workspace.ValidationProcessOwnerFromContext(ctx)
	const commands = 3
	for i := range commands {
		var scratch string
		if i != 0 {
			scratch = filepath.Join(root, "worker-scratch")
		}
		process, err := owner(ctx, workspace.Info{Path: root}, workspace.Issue{}, scratch)
		if err != nil {
			t.Fatal(err)
		}
		if err := process.Started(ctx, procgroup.Identity{PID: os.Getpid(), GroupID: os.Getpid(), StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := process.Finished(ctx, gate.CommandResult{ExitCode: 0}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if writes := sessions.startCalls + sessions.finishCalls + len(sessions.workerProcesses) + sessions.reaps; writes != 2*commands {
		t.Fatalf("%d host commands wrote %d session mutations, want %d", commands, writes, 2*commands)
	}
}
