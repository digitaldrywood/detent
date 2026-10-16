package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestManagedCacheWorkerAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cache, err := toolcache.NewManaged(ctx, root, filepath.Join(root, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "00", fmt.Sprintf("%064x-d", 1))
	if err := os.MkdirAll(filepath.Dir(entry), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
		t.Fatal(err)
	}
	buildStarted, finishBuild := make(chan struct{}), make(chan struct{})
	firstWorkspace := &cacheAdmissionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, after: make(chan struct{}), finish: make(chan struct{})}
	secondWorkspace := &cacheAdmissionWorkspace{fakeWorkspaceBackend: &fakeWorkspaceBackend{info: workspace.Info{Path: t.TempDir()}}, created: make(chan struct{})}
	newRunner := func(ws workspace.Backend, backend AgentBackend) *Runner {
		t.Helper()
		r, err := NewRunner(Dependencies{BuildCache: cache, Workflow: config.Workflow{Prompt: "Work"}, Workspace: ws, AgentBackend: backend})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := newRunner(firstWorkspace, &cacheAdmissionBackend{fakeCodexClient: &fakeCodexClient{}, started: buildStarted, finish: finishBuild})
	second := newRunner(secondWorkspace, &fakeCodexClient{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Run(ctx, RunRequest{Issue: connector.Issue{ID: "first", Identifier: "owner/repository#1"}})
		firstDone <- err
	}()
	defer func() {
		close(firstWorkspace.finish)
		cancel()
		<-firstDone
	}()
	select {
	case <-buildStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	maintenanceDone := make(chan error, 1)
	go func() {
		_, err := cache.Maintain(ctx, toolcache.Policy{MaxBytes: 1024}, time.Now())
		maintenanceDone <- err
	}()
	defer func() { cancel(); <-maintenanceDone }()
	for {
		probe, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		_, release, err := cache.Use(probe)
		stop()
		if errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				t.Fatal("maintenance did not acquire admission before the fixture deadline")
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("maintenance evicted protected build entry: %v", err)
	}
	secondDone := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := second.Run(ctx, RunRequest{Issue: connector.Issue{ID: "second", Identifier: "owner/repository#2"}})
		secondDone <- err
	}()
	defer func() { cancel(); <-secondDone }()
	select {
	case <-secondWorkspace.created:
		t.Logf("second worker reached workspace setup after %s while first build remained protected", time.Since(started))
	case <-time.After(time.Second):
		t.Fatal("second worker stalled before workspace setup behind whole-attempt cache admission")
	}
	close(finishBuild)
	select {
	case <-firstWorkspace.after:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-maintenanceDone:
		maintenanceDone <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("maintenance waited for the original attempt's non-build epilogue")
	}
	if _, err := os.Stat(entry); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprotected over-budget entry retained: %v", err)
	}
	select {
	case err := <-secondDone:
		secondDone <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

type cacheAdmissionWorkspace struct {
	*fakeWorkspaceBackend
	created chan struct{}
	after   chan struct{}
	finish  chan struct{}
}

func (w *cacheAdmissionWorkspace) Create(ctx context.Context, issue workspace.Issue) (workspace.Info, error) {
	if w.created != nil {
		close(w.created)
	}
	return w.fakeWorkspaceBackend.Create(ctx, issue)
}

func (w *cacheAdmissionWorkspace) AfterRun(ctx context.Context, info workspace.Info, issue workspace.Issue) {
	if w.after != nil {
		close(w.after)
		select {
		case <-w.finish:
		case <-ctx.Done():
		}
	}
	w.fakeWorkspaceBackend.AfterRun(ctx, info, issue)
}

type cacheAdmissionBackend struct {
	*fakeCodexClient
	started chan struct{}
	finish  chan struct{}
}

func (b *cacheAdmissionBackend) RunTurn(ctx context.Context, req AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	close(b.started)
	select {
	case <-b.finish:
		return b.fakeCodexClient.RunTurn(ctx, req, update)
	case <-ctx.Done():
		return AgentTurnResult{}, ctx.Err()
	}
}

func TestWithGoBudgetMergesWorkerEnvironment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "budget")
	budget := gobudget.New(8, dir, "/opt/detent")
	toolexec := "-toolexec=" + budget.WrapperPath()
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	goModule := t.TempDir()
	if err := os.WriteFile(filepath.Join(goModule, "go.mod"), []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonGo := t.TempDir()
	monorepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(monorepo, "apps", "api"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(monorepo, "apps", "api", "go.mod"), []byte("module example.com/api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	managedContext, release, err := cache.Use(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	tests := []struct {
		name      string
		workspace string
		budget    gobudget.Budget
		hostFlags string
		hostProcs string
		variables map[string]string
		want      map[string]string
		managed   bool
	}{
		{
			name:      "disabled budget leaves environment",
			workspace: goModule,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "disabled budget preserves nested module overrides",
			workspace: monorepo,
			hostFlags: "-mod=mod",
			hostProcs: "1",
			variables: map[string]string{"GOFLAGS": "-p=3", "GOMAXPROCS": "3"},
			want:      map[string]string{"GOFLAGS": "-p=3", "GOMAXPROCS": "3"},
		},
		{
			name:      "host GOFLAGS are preserved",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want: map[string]string{
				"A": "1", "GOFLAGS": "-mod=mod -p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "host process limit is preserved",
			workspace: monorepo,
			budget:    budget,
			hostProcs: "1",
			want: map[string]string{
				"GOFLAGS": "-p=2 -trimpath " + toolexec, "GOMAXPROCS": "1",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "worker GOFLAGS and GOMAXPROCS win",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			hostProcs: "1",
			variables: map[string]string{"GOFLAGS": "-p=1", "GOMAXPROCS": "3"},
			want: map[string]string{
				"GOFLAGS": "-p=1 -trimpath " + toolexec, "GOMAXPROCS": "3",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "workspace without root module retains budget for subprocesses",
			workspace: nonGo,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want: map[string]string{
				"A": "1", "GOFLAGS": "-mod=mod -p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "nested Go module retains budget",
			workspace: monorepo,
			budget:    budget,
			want: map[string]string{
				"GOFLAGS": "-p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "managed cache reaches non-Go workspace",
			workspace: nonGo,
			managed:   true,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1", "GOCACHE": cacheRoot},
		},
		{
			name:      "missing workspace leaves environment",
			budget:    budget,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "unavailable budget fails open",
			workspace: goModule,
			budget:    gobudget.New(2, filepath.Join(blocked, "budget"), "/opt/detent"),
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{
				goBudget: tt.budget,
				logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				lookupEnv: func(key string) string {
					return map[string]string{"GOFLAGS": tt.hostFlags, "GOMAXPROCS": tt.hostProcs}[key]
				},
			}
			input := procgroup.Environment{Variables: tt.variables, PathPrefixes: []string{"/shim"}}

			ctx := t.Context()
			if tt.managed {
				ctx = managedContext
			}
			got := r.withGoBudget(ctx, tt.workspace, input)

			if runtime.GOOS != "windows" {
				command := exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\n' "$GOFLAGS" "$GOMAXPROCS" "$DETENT_GO_BUDGET_DIR" "$DETENT_GO_BUDGET_SLOTS"`)
				command.Dir = tt.workspace
				if tt.workspace == monorepo {
					command.Dir = filepath.Join(monorepo, "apps", "api")
				}
				procgroup.SetEnvironment(command, procgroup.Environment{Variables: map[string]string{
					"GOFLAGS": tt.hostFlags, "GOMAXPROCS": tt.hostProcs,
					gobudget.DirEnvironment: "", gobudget.SlotsEnvironment: "",
				}})
				procgroup.SetEnvironment(command, got)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("worker child failed: %v: %s", err, output)
				}
				wantFlags, wantProcs := tt.hostFlags, tt.hostProcs
				if value, ok := tt.want["GOFLAGS"]; ok {
					wantFlags = value
				}
				if value, ok := tt.want["GOMAXPROCS"]; ok {
					wantProcs = value
				}
				want := strings.Join([]string{wantFlags, wantProcs, tt.want[gobudget.DirEnvironment], tt.want[gobudget.SlotsEnvironment], ""}, "\n")
				if string(output) != want {
					t.Fatalf("worker child environment = %q, want %q", output, want)
				}
			}
			if len(got.Variables) != len(tt.want) {
				t.Fatalf("variables = %v, want %v", got.Variables, tt.want)
			}
			for key, value := range tt.want {
				if got.Variables[key] != value {
					t.Fatalf("variables[%s] = %q, want %q (all %v)", key, got.Variables[key], value, got.Variables)
				}
			}
			if len(got.PathPrefixes) != 1 || got.PathPrefixes[0] != "/shim" {
				t.Fatalf("PathPrefixes = %v, want preserved", got.PathPrefixes)
			}
		})
	}
}
