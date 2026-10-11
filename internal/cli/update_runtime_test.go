package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type urgentRuntimeUpdater struct {
	apply func(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error)
}

func (u urgentRuntimeUpdater) Check(context.Context) (detentupdate.Status, error) {
	return detentupdate.Status{}, nil
}

func (u urgentRuntimeUpdater) Apply(ctx context.Context, opts detentupdate.ApplyOptions) (detentupdate.Status, error) {
	return u.apply(ctx, opts)
}

func TestEnrolledUpdateOwnerKeepsHeartbeatsAvailable(t *testing.T) {
	for _, test := range []struct {
		name   string
		follow bool
		active int
	}{
		{name: "urgent finishes active leases", active: 2},
		{name: "heartbeat target finishes active leases", follow: true, active: 2},
		{name: "heartbeat target updates idle runner", follow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			follow := test.follow
			runtimeCtx, cancelRuntime := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelRuntime()
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			completed := make(chan struct{})
			restarted := make(chan struct{})
			waiting := make(chan int)
			drains, applies, restarts := 0, 0, 0
			running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: time.Now()}
			scheduler, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{
				Enabled: true, CheckInterval: time.Hour, StatePath: filepath.Join(t.TempDir(), "update.json"), RunningBuild: running,
				Updater: urgentRuntimeUpdater{apply: func(ctx context.Context, opts detentupdate.ApplyOptions) (detentupdate.Status, error) {
					applies++
					if ctx.Err() != nil || opts.Urgent != !follow || opts.FollowHub != follow || opts.ExpectedVersion != "1.2.4" {
						t.Errorf("invalid update context/options: %v %+v", ctx.Err(), opts)
					}
					return detentupdate.Status{Action: detentupdate.ActionUpdated, LatestVersion: "1.2.4", LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}, nil
				}},
				ReserveDrain: func(ctx context.Context) (func(), error) {
					drains++
					for remaining := test.active; remaining > 0; remaining-- {
						waiting <- remaining
						select {
						case <-completed:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return func() {}, nil
				},
				RequestRestart: func(string) bool { restarts++; close(restarted); return true },
			})
			if err != nil {
				t.Fatal(err)
			}
			owner := enrolledUpdateOwner(runtimeCtx, scheduler, running)
			request := &runnerauth.UpdateRequest{RequestedAt: time.Now(), ID: "urgent-test", Service: "detent", Version: "1.2.4", Release: true, Urgent: !follow, FollowHub: follow}
			if follow {
				request.ExpectedBuildRevision = owner(t.Context(), nil).Revision
			}
			owner(requestCtx, request)
			cancelRequest()
			for range test.active {
				select {
				case <-waiting:
				case <-runtimeCtx.Done():
					t.Fatal("urgent delivery did not wait for both sessions")
				}
				observed := owner(t.Context(), request)
				if observed.Receipt == nil || observed.Receipt.Status != "draining" || drains != 1 || applies != 0 || restarts != 0 {
					t.Fatalf("heartbeat during active sessions: %+v drains/applies/restarts=%d/%d/%d", observed, drains, applies, restarts)
				}
				completed <- struct{}{}
			}
			select {
			case <-restarted:
			case <-runtimeCtx.Done():
				t.Fatal("heartbeat update did not restart")
			}
			if _, err := scheduler.ApplyPending(t.Context()); !errors.Is(err, detentupdate.ErrNoPendingUpdate) {
				t.Fatalf("pending apply after completed urgent update: %v", err)
			}
			observed := owner(t.Context(), nil)
			if drains != 1 || applies != 1 || restarts != 1 || observed.Receipt.Status != "restart_requested" {
				t.Fatalf("finished drain: %+v drains/applies/restarts=%d/%d/%d", observed, drains, applies, restarts)
			}
		})
	}
}

func TestHubRuntimeUpdateSchedule(t *testing.T) {
	t.Parallel()
	identityPath := filepath.Join(t.TempDir(), "private", "runner.json")
	identity, err := runnerauth.Initialize(identityPath, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	identity.Identity.OrganizationID = "org_example"
	if err := runnerauth.Save(identityPath, identity); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		client   globalconfig.HubClient
		hours    int
		interval time.Duration
		enrolled bool
	}{
		{name: "Hub heartbeat default", client: globalconfig.HubClient{URL: "https://hub.example.test"}, interval: 6 * time.Hour},
		{name: "Hub configured heartbeat", client: globalconfig.HubClient{URL: "https://hub.example.test", HeartbeatIntervalSeconds: 45}, interval: 6 * time.Hour},
		{name: "Hub explicit check interval", client: globalconfig.HubClient{URL: "https://hub.example.test"}, hours: 2, interval: 2 * time.Hour},
		{name: "enrolled follows Hub through heartbeat owner", client: globalconfig.HubClient{URL: "https://hub.example.test", IdentityFile: identityPath}, interval: 30 * time.Second, enrolled: true},
		{name: "standalone default", interval: 6 * time.Hour},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := BootConfig{Version: "0.117.35", Global: globalconfig.Config{
				Path:   filepath.Join(t.TempDir(), "config.yaml"),
				Client: test.client,
				Update: globalconfig.Update{AutoCheckEnabled: true, AutoApplyEnabled: true, CheckIntervalHours: test.hours},
			}}
			scheduler, err := newRuntimeUpdateScheduler(cfg, nil,
				func(context.Context) (func(), bool) { return func() {}, true },
				func(context.Context) (func(), error) { return func() {}, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if status := scheduler.Status(); status.Enabled == test.enrolled || status.AutoApplyEnabled == test.enrolled || status.CheckInterval != test.interval {
				t.Fatalf("Status() = %#v, want automatic update interval %s", status, test.interval)
			}
		})
	}
}

func TestRuntimeUpdateIdleIsConservative(t *testing.T) {
	t.Parallel()

	if runtimeUpdateIdle(context.Background(), nil) {
		t.Fatal("runtimeUpdateIdle() with nil registry = true, want false")
	}
	if !runtimeUpdateIdle(context.Background(), project.NewRegistry()) {
		t.Fatal("runtimeUpdateIdle() with empty registry = false, want true")
	}
	t.Run("restored completion still owns pending work", func(t *testing.T) {
		if testing.Short() {
			t.Skip("durable completion restore integration")
		}
		now := time.Now()
		runtimeStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = runtimeStore.Close() })
		issue := connector.Issue{ID: "completed-provider", State: "In Progress"}
		attemptID, err := runtimeStore.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "fixture", IssueID: issue.ID, WorkerType: "implement", Lane: issue.State, AttemptNumber: 1, StartedAt: now, LeaseExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := json.Marshal(map[string]any{"deferred_completion": map[string]any{"schema": 1, "running": orchestrator.Running{Issue: issue, WorkAttemptID: attemptID, Attempt: 1}, "completed_at": now, "deferred_at": now, "fence_retry_at": now.Add(time.Hour)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := runtimeStore.RecordWorkAttemptHeartbeat(t.Context(), store.WorkAttemptHeartbeat{AttemptID: attemptID, HeartbeatAt: now, Phase: "completion_deferred", WorkerMetadataJSON: string(metadata)}); err != nil {
			t.Fatal(err)
		}
		cfg := workflowconfig.Default()
		cfg.Tracker.Kind = workflowconfig.TrackerMemory
		tracked, err := project.New(project.Config{Project: globalconfig.Project{ID: "fixture", Workdir: t.TempDir(), Weight: 1}, Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."}}, project.Dependencies{WorkAttempts: runtimeStore})
		if err != nil {
			t.Fatal(err)
		}
		if err := tracked.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := tracked.Stop(ctx); err != nil {
				t.Error(err)
			}
		})
		registry := project.NewRegistry()
		if err := registry.Set(tracked); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var state orchestrator.State
		for {
			state, err = tracked.Orchestrator().State(ctx)
			if err != nil || state.UnsettledWork() > 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("completion owner was not restored")
			case <-time.After(time.Millisecond):
			}
		}
		if err != nil || len(state.Snapshot(now).Running) != 0 || state.UnsettledWork() != 1 {
			t.Fatalf("completion owner unavailable: unsettled=%d err=%v", state.UnsettledWork(), err)
		}
		if runtimeUpdateIdle(t.Context(), registry) {
			t.Fatal("updater treated pending completion as idle")
		}
	})
}

func TestRuntimeUpdateIdleReservationBlocksDispatch(t *testing.T) {
	t.Parallel()

	candidates := []scheduler.ProjectCandidate{{ID: "detent"}, {ID: "video", Pool: "video"}}
	gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{
		{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}},
		{Name: "video", Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}},
	}, candidates)
	if err != nil {
		t.Fatalf("NewPoolRegistry() error = %v", err)
	}
	if release, ok := runtimeUpdateIdleReservation(context.Background(), nil, gate); ok || release != nil {
		t.Fatalf("runtimeUpdateIdleReservation() with nil registry ok = %t release nil = %t, want false/true", ok, release == nil)
	}
	release, ok := runtimeUpdateIdleReservation(context.Background(), project.NewRegistry(), gate)
	if !ok || release == nil {
		t.Fatal("runtimeUpdateIdleReservation() did not reserve an idle runtime")
	}
	request := scheduler.SlotRequest{State: "Todo"}
	for _, candidate := range candidates {
		if _, acquired, decision, err := gate.TryAcquireWithDecision(context.Background(), candidate, request, time.Now()); err != nil {
			t.Fatalf("TryAcquireWithDecision(%s) while reserved error = %v", candidate.ID, err)
		} else if acquired || decision.Reason != scheduler.DispatchGateReasonPaused {
			t.Fatalf("TryAcquireWithDecision(%s) while reserved acquired = %t decision = %#v", candidate.ID, acquired, decision)
		}
	}

	release()
	for _, candidate := range candidates {
		slot, acquired, err := gate.TryAcquire(context.Background(), candidate, request, time.Now())
		if err != nil {
			t.Fatalf("TryAcquire(%s) after release error = %v", candidate.ID, err)
		}
		if !acquired {
			t.Fatalf("TryAcquire(%s) after release acquired = false, want true", candidate.ID)
		}
		if err := gate.Release(slot); err != nil {
			t.Fatalf("Release(%s) error = %v", candidate.ID, err)
		}
	}
}

func TestRequestUpdateRestartUsesShutdownDrainState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		prepare        func(*ShutdownController)
		wantAccepted   bool
		wantBinary     string
		wantDrainEvent bool
	}{
		{
			name:           "idle controller accepts update restart",
			wantAccepted:   true,
			wantBinary:     "/opt/detent/bin/detent",
			wantDrainEvent: true,
		},
		{
			name: "manual drain already in progress",
			prepare: func(controller *ShutdownController) {
				controller.RequestDrain()
			},
			wantDrainEvent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			controller := NewShutdownController()
			deactivate := controller.activate()
			t.Cleanup(deactivate)
			if tt.prepare != nil {
				tt.prepare(controller)
			}
			restart := NewRestartRequest()
			accepted := requestUpdateRestart(controller, restart, "/opt/detent/bin/detent")
			if accepted != tt.wantAccepted {
				t.Fatalf("requestUpdateRestart() = %t, want %t", accepted, tt.wantAccepted)
			}
			if got := restart.Binary(); got != tt.wantBinary {
				t.Fatalf("Binary() = %q, want %q", got, tt.wantBinary)
			}
			select {
			case request := <-controller.Requests():
				if !tt.wantDrainEvent || request != ShutdownRequestDrain {
					t.Fatalf("shutdown request = %v, want drain=%t", request, tt.wantDrainEvent)
				}
			default:
				if tt.wantDrainEvent {
					t.Fatal("shutdown request missing, want drain")
				}
			}
		})
	}
}

func TestWaitForRuntimeUpdateIdleHonorsCeiling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		idleAfter time.Duration
		wantWait  time.Duration
		wantErr   error
	}{
		{name: "in-flight attempts finish before ceiling", idleAfter: time.Second, wantWait: time.Second},
		{name: "in-flight attempts consume full ceiling", idleAfter: 4 * time.Second, wantWait: 3 * time.Second, wantErr: ErrRuntimeUpdateDrainTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			startedAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			now := startedAt
			waited := time.Duration(0)
			err := waitForRuntimeUpdateIdle(
				context.Background(),
				func(context.Context) bool { return now.Sub(startedAt) >= tt.idleAfter },
				3*time.Second,
				func() time.Time { return now },
				func(_ context.Context, delay time.Duration) bool {
					now = now.Add(delay)
					waited += delay
					return true
				},
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("waitForRuntimeUpdateIdle() error = %v, want %v", err, tt.wantErr)
			}
			if waited != tt.wantWait {
				t.Fatalf("waited = %s, want %s", waited, tt.wantWait)
			}
		})
	}
}

func TestRequestUpdateRestartRacesManualDrainWithoutOverwritingOwner(t *testing.T) {
	t.Parallel()

	for range 100 {
		controller := NewShutdownController()
		deactivate := controller.activate()
		restart := NewRestartRequest()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			controller.RequestDrain()
		}()
		go func() {
			defer wg.Done()
			<-start
			requestUpdateRestart(controller, restart, "/opt/detent/bin/detent")
		}()
		close(start)
		wg.Wait()
		if restart.Binary() != "" && restart.Binary() != "/opt/detent/bin/detent" {
			t.Fatalf("Binary() = %q, want empty or update binary", restart.Binary())
		}
		deactivate()
	}
}

func TestDrainCeilingIncludesSelectedSessionLevels(t *testing.T) {
	t.Parallel()
	for _, hours := range []int{12, 24} {
		t.Run((time.Duration(hours) * time.Hour).String(), func(t *testing.T) {
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerMemory
			duration := hours * 60 * 60 * 1000
			cfg.Agents.ModelSelection.Levels = map[string]workflowconfig.ModelSelectionDefaults{"high": {MaxSessionDurationMS: &duration}}
			tracked := newShutdownRuntimeProjectWithConfig(t, "fixture", cfg, orchestrator.FakeRunner{})
			registry := project.NewRegistry()
			if err := registry.Set(tracked); err != nil {
				t.Fatal(err)
			}
			if got := shutdownDrainTimeout(registry); got != time.Duration(duration)*time.Millisecond {
				t.Fatalf("ceiling = %s", got)
			}
		})
	}
}
