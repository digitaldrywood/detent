package orchestrator

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func finishTestWorkspaceCleanup(t *testing.T, o *Orchestrator, state *State) {
	t.Helper()
	if o.workspaceCleanupCancel == nil {
		return
	}
	select {
	case result := <-o.workspaceCleanupResults:
		o.finishWorkspaceCleanup(state, result)
	case <-time.After(10 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	o.workspaceCleanupWG.Wait()
}

func TestWorkspaceCleanupBatch(t *testing.T) {
	for _, count := range []int{0, 1, 10, 11, 50, 135} {
		for _, preserved := range []bool{false, true} {
			t.Run(fmt.Sprintf("count=%d/preserved=%v", count, preserved), func(t *testing.T) {
				cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}, WorkspaceCleanupSweepInterval: time.Hour})
				tracker := &runningStateConnector{}
				for i := range count {
					tracker.issuesByState = append(tracker.issuesByState, connector.Issue{ID: fmt.Sprintf("%03d", i), Identifier: fmt.Sprintf("repo#%d", i), State: "Done"})
				}
				reaper := &cleanupSweepReaper{}
				if preserved {
					reaper.err = workspace.ErrWorkspacePreserved
				}
				o := &Orchestrator{cfg: cfg, connector: tracker, reaper: reaper, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				state := newState(cfg)
				now := time.Now()
				for pass := range 2 {
					start := len(reaper.issues)
					o.startWorkspaceCleanup(t.Context(), &state, now.Add(time.Duration(pass)*time.Hour))
					finishTestWorkspaceCleanup(t, o, &state)
					want := min(count, workspaceCleanupBatchSize)
					if !preserved {
						want = min(max(0, count-pass*workspaceCleanupBatchSize), workspaceCleanupBatchSize)
					}
					if got := len(reaper.issues) - start; got != want {
						t.Fatalf("pass %d calls=%d want=%d", pass, got, want)
					}
				}
				if count > workspaceCleanupBatchSize && len(reaper.issues) > workspaceCleanupBatchSize && reaper.issues[0].ID == reaper.issues[workspaceCleanupBatchSize].ID {
					t.Fatal("retained candidates starved the next batch")
				}
			})
		}
	}
}

func TestWorkspaceCleanupResultPreservesNewOwner(t *testing.T) {
	cfg := Config{}
	before := newState(cfg)
	after := before.clone()
	after.ReapedWorkspaces["issue"] = time.Now()
	current := before.clone()
	current.Running["issue"] = Running{}
	current.CleanupFailures["new"] = "new failure"
	o := &Orchestrator{workspaceCleanupCancel: func() {}}
	o.finishWorkspaceCleanup(&current, workspaceCleanupResult{before: before, after: after})
	if _, ok := current.ReapedWorkspaces["issue"]; ok {
		t.Fatal("old sweep marked a replacement worker as reaped")
	}
	if current.CleanupFailures["new"] != "new failure" {
		t.Fatal("sweep lost concurrent failure")
	}
}
