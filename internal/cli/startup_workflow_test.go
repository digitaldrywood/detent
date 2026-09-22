package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
			projects := []globalconfig.Project{
				{ID: "healthy", Workflow: healthyPath, Workdir: filepath.Dir(healthyPath), Weight: 1},
				{ID: "invalid", Workflow: invalidPath, Workdir: filepath.Dir(invalidPath), Weight: 1},
			}
			if invalidFirst {
				projects[0], projects[1] = projects[1], projects[0]
			}
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
				waitForProjectDataSeq(t, healthy, 1)
				snapshots := hub.New[telemetry.Snapshot]()
				var seq atomic.Uint64
				if err := publishSnapshotOnce(t.Context(), manager.Registry(), nil, snapshots, &seq, nil, time.Now(), nil, nil, "", nil); err != nil {
					t.Fatal(err)
				}
				snapshot, ok := snapshots.Latest()
				if !ok || len(snapshot.Projects) != 2 {
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
					return workflowconfig.Workflow{}, errors.New("invalid workflow")
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
