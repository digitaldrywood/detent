package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type failingRetentionReaper struct {
	cleanupSweepReaper
	err error
}

type localRetentionReaper struct {
	cleanupSweepReaper
	*workspace.LocalGit
}

func (r *failingRetentionReaper) SweepRetention(_ context.Context, request workspace.RetentionRequest) (workspace.RetentionTotals, error) {
	return workspace.RetentionTotals{Workdir: "test", At: request.Now}, r.err
}

func TestRetentionQuarantineWarningOncePerPath(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		paths []string
	}{
		{name: "one unremovable path", paths: []string{".detent/quarantine/old-a"}},
		{name: "two unremovable paths", paths: []string{".detent/quarantine/old-a", ".detent/quarantine/old-b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var failures []error
			for _, path := range test.paths {
				failures = append(failures, &os.PathError{Op: "RemoveAll", Path: path, Err: fs.ErrPermission})
			}
			var logs bytes.Buffer
			o := &Orchestrator{
				reaper: &failingRetentionReaper{err: errors.Join(failures...)},
				logger: slog.New(slog.NewTextHandler(&logs, nil)),
			}
			for range 3 {
				o.sweepRetention(t.Context(), &State{}, time.Now())
			}
			if got := strings.Count(logs.String(), "workspace retention failed"); got != len(test.paths) {
				t.Fatalf("warnings=%d want=%d: %s", got, len(test.paths), logs.String())
			}
			for _, path := range test.paths {
				if got := strings.Count(logs.String(), path); got != 1 {
					t.Fatalf("warnings for %s=%d: %s", path, got, logs.String())
				}
			}
		})
	}
}

func TestRetentionUnremovableQuarantineWarnsOnce(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspaces")
	backend, err := workspace.NewLocalGit(workspace.LocalGitOptions{Root: root, SourceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	parent := filepath.Join(root, ".detent/quarantine")
	quarantine := filepath.Join(parent, "workspace-20260910T120000.000000000Z")
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quarantine, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o700); err != nil {
			t.Error(err)
		}
	})
	var logs bytes.Buffer
	cfg := normalizeConfig(Config{WorkspaceCleanupSweepInterval: time.Hour})
	o := &Orchestrator{cfg: cfg, connector: memory.New(memory.Config{}), reaper: &localRetentionReaper{LocalGit: backend}, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	state := newState(cfg)
	for pass := range 3 {
		o.startWorkspaceCleanup(t.Context(), &state, now.Add(time.Duration(pass)*time.Hour))
		finishTestWorkspaceCleanup(t, o, &state)
	}
	if got := strings.Count(logs.String(), "workspace retention failed"); got != 1 {
		t.Fatalf("warnings=%d want=1: %s", got, logs.String())
	}
	if !strings.Contains(logs.String(), filepath.Join(".detent/quarantine", filepath.Base(quarantine))) {
		t.Fatalf("warning missing quarantine path: %s", logs.String())
	}
}

type retentionTestReaper struct {
	cleanupSweepReaper
	completed map[string]time.Time
}

func (r *retentionTestReaper) SweepRetention(ctx context.Context, request workspace.RetentionRequest) (workspace.RetentionTotals, error) {
	completed, err := request.Completed(ctx, []workspace.Issue{{ID: "issue"}})
	r.completed = completed
	return workspace.RetentionTotals{Workdir: "test", At: request.Now, HookLogs: workspace.RemovalTotal{Count: 2, Bytes: 32}}, err
}

func TestRetentionCompletionClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	done := now.Add(-8 * 24 * time.Hour)
	closed := now.Add(-7 * 24 * time.Hour)
	for _, test := range []struct {
		name, state              string
		closed, stage, closeTime bool
		want                     time.Time
	}{
		{name: "done", state: "Done", stage: true, want: done},
		{name: "cancelled", state: "Cancelled", stage: true, want: done},
		{name: "closed lane", state: "Closed", stage: true, want: done},
		{name: "duplicate lane", state: "Duplicate", stage: true, want: done},
		{name: "custom terminal", state: "Archived", stage: true, want: done},
		{name: "custom terminal unknown time", state: "Archived"},
		{name: "closed later comment", state: "Backlog", closed: true, closeTime: true, want: closed},
		{name: "done then closed", state: "Done", stage: true, closed: true, closeTime: true, want: done},
		{name: "unknown time", state: "Done"},
		{name: "active", state: "In Progress", stage: true},
		{name: "human review", state: "Human Review", stage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := connector.Issue{ID: "issue", Identifier: "repo#1", State: test.state, Closed: test.closed, UpdatedAt: &now}
			if test.stage {
				issue.StageUpdatedAt = &done
			}
			if test.closeTime {
				issue.ClosedAt = &closed
			}
			reaper := &retentionTestReaper{}
			o := &Orchestrator{cfg: Config{TerminalStates: normalizedStates([]string{"Done", "Cancelled", "Canceled", "Closed", "Duplicate", "Archived"})}, reaper: reaper, connector: memory.New(memory.Config{Issues: []connector.Issue{issue}}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			state := State{}
			o.sweepRetention(t.Context(), &state, now)
			if got := reaper.completed[issue.ID]; !got.Equal(test.want) {
				t.Fatalf("completed=%v want=%v", got, test.want)
			}
			if len(state.WorkspaceRetention) != 1 || state.WorkspaceRetention[0].HookLogs.Count != 2 {
				t.Fatalf("totals=%+v", state.WorkspaceRetention)
			}
		})
	}
}
