package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStartupIsolatesWorkflowLoadFailure(t *testing.T) {
	t.Parallel()
	for _, invalidFirst := range []bool{true, false} {
		name := "invalid last"
		if invalidFirst {
			name = "invalid first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			healthyPath := writeWorkflowFile(t)
			invalidPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
			content := "---\ntracker:\n  kind: memory\nbacklog_admission:\n  enabled: true\n  criteria_section: Admission Criteria\n---\n## Renamed Criteria\nAccept bugs.\n"
			if err := os.WriteFile(invalidPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			pausedPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if err := os.WriteFile(pausedPath, []byte("---\ntracker:\n  kind: github\n  repository: owner/paused\n  api_key: test-token\n  github_status_source: label\n  status_label_prefix: 'detent:'\n---\nWork.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			projects := []globalconfig.Project{
				{ID: "healthy", Workflow: healthyPath, Workdir: filepath.Dir(healthyPath), Weight: 1},
				{ID: "invalid", Workflow: invalidPath, Workdir: filepath.Dir(invalidPath), Weight: 1},
			}
			if invalidFirst {
				projects[0], projects[1] = projects[1], projects[0]
			}
			projects = append(projects, globalconfig.Project{ID: "paused", Workflow: pausedPath, Workdir: filepath.Dir(pausedPath), Weight: 1, Paused: true, PausedUntilIssue: "owner/invalid#1"})
			const wantError = `backlog admission criteria section "Admission Criteria" was not found in WORKFLOW.md`
			t.Run("attribution", func(t *testing.T) {
				if err := backfillRuntimeSessionProjects(t.Context(), projects, &fakeSessionProjectBackfiller{}, project.LoadWorkflow); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("manager", func(t *testing.T) {
				run := &workflowStartupRunner{started: make(chan struct{}, 1)}
				tracker := memory.New(memory.Config{Issues: []connector.Issue{{ID: "1", Identifier: "TEST-1", Title: "Healthy work", State: "Todo", AssignedToWorker: true}}})
				manager, err := project.NewManager(project.ManagerConfig{Projects: projects}, project.ManagerDependencies{
					ProjectFactory: withRunnerFactory(project.Dependencies{Runner: run, Connector: tracker, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil, nil, serviceapi.Connection{}, nil),
				})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if err := manager.Start(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cancel()
					for _, p := range manager.Registry().List() {
						if err := p.Close(); err != nil {
							t.Error(err)
						}
					}
					manager.Wait()
				})
				pending, ok := manager.Registry().Pending("invalid")
				if !ok || !pending.RetryStopped || !pending.NextRetryAt.IsZero() || !strings.Contains(pending.LastError, wantError) {
					t.Fatalf("invalid project health = %+v, found = %v", pending, ok)
				}
				select {
				case <-run.started:
				case <-time.After(5 * time.Second):
					t.Fatal("healthy project did not dispatch")
				}
				healthy, ok := manager.Registry().Get("healthy")
				if !ok {
					t.Fatal("healthy project missing")
				}
				paused, ok := manager.Registry().Get("paused")
				if !ok || !paused.Config().Paused {
					t.Fatal("dependent project must stay paused")
				}
				checkPauseExitConditions(t.Context(), pauseMonitorDeps{
					read:    func() (globalconfig.Config, error) { return globalconfig.Config{Projects: projects}, nil },
					write:   func(globalconfig.Config) error { t.Fatal("unresolved pause must not update config"); return nil },
					unpause: func(context.Context, string) error { t.Fatal("unresolved pause must not unpause"); return nil },
					trackerKindFor: func(id string) string {
						if p, ok := manager.Registry().Get(project.ID(id)); ok {
							return p.Workflow().Config.Tracker.Kind
						}
						return ""
					},
					repositoryFor: func(id string) string {
						if p, ok := manager.Registry().Get(project.ID(id)); ok {
							return p.Workflow().Config.Tracker.Repository
						}
						return ""
					},
					connectorFor: func(id string) connector.Connector {
						if p, ok := manager.Registry().Get(project.ID(id)); ok {
							return p.Connector()
						}
						return nil
					},
					setPauseStatus:   manager.Registry().SetPauseExitStatus,
					pauseStatus:      manager.Registry().PauseExitStatus,
					clearPauseStatus: manager.Registry().ClearPauseExitStatus,
				})
				status, ok := manager.Registry().PauseExitStatus("paused")
				if !ok || status.Evaluable || !strings.Contains(status.LastError, "owner/invalid#1") {
					t.Fatalf("pause status = %+v, found = %v", status, ok)
				}
				if _, err := manager.Reconcile(t.Context(), project.ManagerConfig{Projects: projects}); err != nil {
					t.Fatalf("reload with unavailable pause target: %v", err)
				}
				if !paused.Config().Paused {
					t.Fatal("reload cleared unresolved pause")
				}
				waitForProjectDataSeq(t, healthy, 1)
				snapshots := hub.New[telemetry.Snapshot]()
				var seq atomic.Uint64
				if err := publishSnapshotOnce(t.Context(), manager.Registry(), nil, snapshots, &seq, nil, time.Now(), nil, nil, "", nil); err != nil {
					t.Fatal(err)
				}
				snapshot, ok := snapshots.Latest()
				if !ok || len(snapshot.Projects) != 3 {
					t.Fatalf("missing project snapshots: %+v", snapshot.Projects)
				}
				for _, p := range snapshot.Projects {
					if p.Project.ID == "invalid" && (!p.Refresh.Degraded() || p.Tracker.Available() || !strings.Contains(p.Refresh.LastError, wantError)) {
						t.Fatalf("invalid dashboard snapshot = %+v", p)
					}
					if p.Project.ID == "healthy" && !p.Tracker.Available() {
						t.Fatalf("healthy dashboard unavailable: %+v", p)
					}
				}
			})
		})
	}
}

func TestStartupIsolatesWorkspacePathFailureAndReloads(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		badRoot bool
	}{
		{name: "workspace root cannot be created", badRoot: true},
		{name: "source root cannot be resolved"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			blockedHome := filepath.Join(root, "home")
			if err := os.WriteFile(blockedHome, []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			badPath := filepath.Join(blockedHome, "user", "workspaces")
			goodPath := filepath.Join(root, "workspaces")
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			writeWorkflow := func(workspaceRoot, sourceRoot string) {
				t.Helper()
				content := "---\ntracker:\n  kind: memory\ncodex:\n  command: codex app-server\nworkspace:\n  root: " + workspaceRoot + "\n  source_root: " + sourceRoot + "\n---\n\nWork.\n"
				if err := os.WriteFile(workflowPath, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.badRoot {
				writeWorkflow(badPath, root)
			} else {
				writeWorkflow(goodPath, badPath)
			}

			healthyPath := writeWorkflowFile(t)
			projects := []globalconfig.Project{
				{ID: "invalid", Workflow: workflowPath, Workdir: root, Weight: 1},
				{ID: "healthy", Workflow: healthyPath, Workdir: filepath.Dir(healthyPath), Weight: 1},
			}
			manager, err := project.NewManager(project.ManagerConfig{Projects: projects}, project.ManagerDependencies{
				ProjectFactory: withRunnerFactory(project.Dependencies{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil, nil, serviceapi.Connection{}, nil),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(t.Context()); err != nil {
				t.Fatalf("Start() error = %v, want isolated project failure", err)
			}
			t.Cleanup(func() {
				for _, p := range manager.Registry().List() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				}
				manager.Wait()
			})
			pending, ok := manager.Registry().Pending("invalid")
			if !ok || !pending.RetryStopped || !strings.Contains(pending.LastError, badPath) || !strings.Contains(pending.LastError, "not a directory") {
				t.Fatalf("invalid project health = %+v, found = %v", pending, ok)
			}
			if healthy, ok := manager.Registry().Get("healthy"); !ok || !healthy.Running() {
				t.Fatalf("healthy project = %v, found = %v, want running", healthy, ok)
			}

			writeWorkflow(goodPath, root)
			if _, err := manager.Reconcile(t.Context(), project.ManagerConfig{Projects: projects}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if _, ok := manager.Registry().Pending("invalid"); ok {
				t.Fatal("invalid project remains pending after path correction")
			}
			if recovered, ok := manager.Registry().Get("invalid"); !ok || !recovered.Running() {
				t.Fatalf("recovered project = %v, found = %v, want running", recovered, ok)
			}
		})
	}
}

type workflowStartupRunner struct{ started chan struct{} }

func (r *workflowStartupRunner) Run(ctx context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

func TestBackfillRuntimeSessionProjectsFailures(t *testing.T) {
	t.Parallel()
	storeErr := errors.New("database write failed")
	for _, tt := range []struct {
		name      string
		invalidID string
		storeErr  error
		wantCalls int
	}{
		{name: "all readable", wantCalls: 1},
		{name: "first unreadable", invalidID: "first"},
		{name: "last unreadable", invalidID: "last"},
		{name: "store error remains fatal", storeErr: storeErr, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			backfiller := &fakeSessionProjectBackfiller{err: tt.storeErr}
			err := backfillRuntimeSessionProjects(t.Context(), []globalconfig.Project{{ID: "first"}, {ID: "last"}}, backfiller, func(p globalconfig.Project) (workflowconfig.Workflow, error) {
				if p.ID == tt.invalidID {
					return workflowconfig.Workflow{}, project.ErrProjectDefinition
				}
				cfg := workflowconfig.Default()
				cfg.Tracker.Repository = "owner/" + p.ID
				return workflowconfig.Workflow{Config: cfg}, nil
			})
			if !errors.Is(err, tt.storeErr) {
				t.Fatalf("error = %v, want %v", err, tt.storeErr)
			}
			if backfiller.calls != tt.wantCalls {
				t.Fatalf("backfill calls = %d, want %d", backfiller.calls, tt.wantCalls)
			}
			if tt.wantCalls == 1 {
				want := []store.SessionProjectAttribution{{ProjectID: "first", Repository: "owner/first"}, {ProjectID: "last", Repository: "owner/last"}}
				if !reflect.DeepEqual(backfiller.attributions, want) {
					t.Fatalf("attributions = %+v, want %+v", backfiller.attributions, want)
				}
			}
		})
	}
}

func TestStartupInfrastructureFailureRemainsFatal(t *testing.T) {
	for _, invalidFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(invalidFirst), func(t *testing.T) {
			invalidPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
			if err := os.WriteFile(invalidPath, []byte("---\ntracker: [\n---\n"), 0600); err != nil {
				t.Fatal(err)
			}
			unavailable := globalconfig.Project{ID: "unavailable", Workdir: filepath.Join(t.TempDir(), "missing"), Workflow: "WORKFLOW.md", WorkflowRef: "HEAD"}
			projects := []globalconfig.Project{unavailable, {ID: "invalid", Workflow: invalidPath}}
			if invalidFirst {
				projects[0], projects[1] = projects[1], projects[0]
			}
			backfiller := &fakeSessionProjectBackfiller{}
			if err := backfillRuntimeSessionProjects(t.Context(), projects, backfiller, project.LoadWorkflow); err == nil || errors.Is(err, project.ErrProjectDefinition) {
				t.Fatalf("backfill error = %v", err)
			}
			if backfiller.calls != 0 {
				t.Fatal("incomplete attribution was written")
			}
			manager, err := project.NewManager(project.ManagerConfig{Projects: projects}, project.ManagerDependencies{ProjectFactory: withRunnerFactory(project.Dependencies{}, nil, nil, serviceapi.Connection{}, nil)})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(t.Context()); err == nil || errors.Is(err, project.ErrProjectDefinition) {
				t.Fatalf("manager error = %v", err)
			}
		})
	}
}
