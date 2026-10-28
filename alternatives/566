package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
	"github.com/digitaldrywood/detent/internal/toolcache"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

type urgentRuntimeUpdater struct {
	apply func(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error)
}

func TestUpdateRestartCancellationPreservesShutdownOwner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		request   func(*ShutdownController)
		kind      ShutdownRequest
		committed bool
	}{
		{name: "runtime cancellation"},
		{name: "operator drain cancels startup", request: (*ShutdownController).RequestDrain, kind: ShutdownRequestDrain},
		{name: "operator force cancels startup", request: (*ShutdownController).RequestForce, kind: ShutdownRequestForce},
		{name: "interrupt cancels startup", request: func(c *ShutdownController) { c.RequestInterrupt() }, kind: ShutdownRequestDrain},
		{name: "committed replacement retains operator drain", request: (*ShutdownController).RequestDrain, kind: ShutdownRequestDrain, committed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				controller := NewShutdownController()
				defer controller.activate()()
				gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				finish := make(chan struct{})
				var finished sync.Once
				defer finished.Do(func() { close(finish) })
				canceled := make(chan struct{})
				restart := &RestartRequest{control: &controllerConnection{}, reserve: gate.PauseDispatch, execute: func(ctx context.Context, _ string) error {
					select {
					case <-ctx.Done():
						close(canceled)
					case <-finish:
					}
					<-finish
					if test.committed {
						return nil
					}
					return context.Canceled
				}}
				done := make(chan struct{})
				var accepted bool
				var restartErr error
				go func() {
					accepted, restartErr = requestRuntimeUpdateRestart(ctx, controller, restart, "verified-controller", nil)
					close(done)
				}()
				synctest.Wait()
				requested := make(chan struct{})
				go func() {
					if test.request != nil {
						test.request(controller)
					} else {
						cancel()
					}
					close(requested)
				}()
				synctest.Wait()
				select {
				case <-requested:
				default:
					t.Fatal("pending adoption blocked the operator shutdown request")
				}
				select {
				case <-canceled:
				default:
					t.Fatal("shutdown did not cancel the pending adoption")
				}
				if release, admitted := gate.ReserveDispatch(); admitted {
					release()
					t.Fatal("admission resumed before replacement rejection")
				}
				finished.Do(func() { close(finish) })
				<-done
				if accepted != test.committed || !test.committed && !errors.Is(restartErr, context.Canceled) || test.committed && restartErr != nil {
					t.Fatalf("restart accepted=%t error=%v, committed=%t", accepted, restartErr, test.committed)
				}
				if controller.updateRestart.Load() || controller.shutdownRequested.Load() != (test.kind != 0) || restart.control.retired.Load() != test.committed {
					t.Fatal("adoption overwrote the operator shutdown owner")
				}
				if test.kind != 0 {
					if got := <-controller.Requests(); got != test.kind {
						t.Fatalf("shutdown request = %v, want %v", got, test.kind)
					}
				}
				release, admitted := gate.ReserveDispatch()
				if admitted {
					release()
				}
				if admitted == test.committed {
					t.Fatalf("admission after restart = %t, committed=%t", admitted, test.committed)
				}
			})
		})
	}
}

func TestUpdateRestartHandsOffWhileAttemptRemainsActive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		release := make(chan struct{})
		var finish sync.Once
		runner := &shutdownBlockingRunner{started: make(chan struct{}, 1), canceled: make(chan struct{}, 1), release: release}
		tracked := newShutdownRuntimeProject(t, "fixture", []connector.Issue{{ID: "active-issue", Identifier: "owner/repository#1", Title: "Active implementation", State: "Todo", AssignedToWorker: true}}, runner)
		if err := tracked.Start(ctx); err != nil {
			t.Fatal(err)
		}
		defer tracked.Close()
		defer finish.Do(func() { close(release) })
		synctest.Wait()
		select {
		case <-runner.started:
		default:
			t.Fatal("fixture did not start the active attempt")
		}
		registry := project.NewRegistry()
		mustSetProject(t, registry, tracked)
		oldGate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{{ID: "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		newGate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{{ID: "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		controller := NewShutdownController()
		replaced := false
		restart := &RestartRequest{control: &controllerConnection{}, reserve: oldGate.PauseDispatch, execute: func(context.Context, string) error {
			if runtimeUpdateIdle(ctx, registry) {
				t.Error("replacement waited for the active attempt")
			}
			replaced = true
			return nil
		}}
		done := make(chan error, 1)
		go func() {
			done <- runWithShutdown(ctx, runningShutdownConfig{Controller: controller, Registry: registry, DrainTimeout: time.Minute, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
		}()
		synctest.Wait()
		if accepted, err := requestRuntimeUpdateRestart(ctx, controller, restart, "verified-controller", nil); !accepted || err != nil {
			t.Fatalf("applied update was not adopted: %t %v", accepted, err)
		}
		synctest.Wait()
		if !replaced || restart.Binary() != "" {
			t.Fatal("supervisor did not replace the controller independently of attempt retirement")
		}
		if releaseClaim, admitted := oldGate.ReserveDispatch(); admitted {
			releaseClaim()
			t.Error("retired controller still claims work")
		}
		if releaseClaim, admitted := newGate.ReserveDispatch(); !admitted {
			t.Fatal("new controller did not resume claiming while the old attempt runs")
		} else {
			releaseClaim()
		}
		select {
		case <-done:
			t.Error("old attempt owner exited before its attempt completed")
		case <-runner.canceled:
			t.Error("controller adoption canceled the active attempt")
		default:
		}
		finish.Do(func() { close(release) })
		if err := <-done; err != nil {
			t.Fatalf("retiring attempt owner: %v", err)
		}
	})
}

func (u urgentRuntimeUpdater) Check(context.Context) (detentupdate.Status, error) {
	return detentupdate.Status{Critical: true, LatestVersion: "1.2.4"}, nil
}

func (u urgentRuntimeUpdater) Apply(ctx context.Context, opts detentupdate.ApplyOptions) (detentupdate.Status, error) {
	return u.apply(ctx, opts)
}

func TestEnrolledUpdateOwnerKeepsHeartbeatsAvailable(t *testing.T) {
	for _, test := range []struct {
		name   string
		follow bool
		active int
		repair bool
	}{
		{name: "critical urgent finishes active leases", active: 2},
		{name: "critical heartbeat target finishes active leases", follow: true, active: 2},
		{name: "critical heartbeat target updates idle runner", follow: true},
		{name: "critical same version repair finishes active leases", follow: true, repair: true, active: 2},
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
			if test.repair {
				running.Version = "1.2.4"
				running.Source = "release"
			}
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
		isolated bool
	}{
		{name: "Hub heartbeat default", client: globalconfig.HubClient{URL: "https://hub.example.test"}, interval: 6 * time.Hour},
		{name: "Hub configured heartbeat", client: globalconfig.HubClient{URL: "https://hub.example.test", HeartbeatIntervalSeconds: 45}, interval: 6 * time.Hour},
		{name: "Hub explicit check interval", client: globalconfig.HubClient{URL: "https://hub.example.test"}, hours: 2, interval: 2 * time.Hour},
		{name: "enrolled follows Hub through heartbeat owner", client: globalconfig.HubClient{URL: "https://hub.example.test", IdentityFile: identityPath}, interval: 30 * time.Second, enrolled: true},
		{name: "standalone default", interval: 6 * time.Hour},
		{name: "isolated demo never prepares released workers", isolated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := BootConfig{Version: "0.117.35", Global: globalconfig.Config{
				Path:   filepath.Join(t.TempDir(), "config.yaml"),
				Client: test.client,
				Update: globalconfig.Update{AutoCheckEnabled: true, AutoApplyEnabled: true, CheckIntervalHours: test.hours},
			}}
			if test.isolated {
				cfg.Isolated = &IsolatedRuntimeInfo{}
			}
			scheduler, err := newRuntimeUpdateScheduler(t.Context(), cfg, nil,
				func(context.Context) (func(), bool) { return func() {}, true },
				func(context.Context) (func(), error) { return func() {}, nil },
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.isolated {
				if status := scheduler.Status(); status.Enabled || status.AutoApplyEnabled || scheduler.WorkerArtifacts() != nil {
					t.Fatalf("isolated runtime has an update owner: %#v", status)
				}
				if err := scheduler.PrepareWorkers(t.Context()); err != nil {
					t.Fatal(err)
				}
				return
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
	t.Run("pending admission maintenance does not block idle adoption", func(t *testing.T) {
		backend, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "runtime.db")})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Close() })
		lookedUp := make(chan struct{}, 1)
		attempts := &pendingAdmissionMaintenanceStore{Store: backend, lookedUp: lookedUp}
		cfg := workflowconfig.Default()
		cfg.Tracker.Kind = workflowconfig.TrackerHubNative
		tracked, err := projectfixture.New(project.Config{
			Project:  globalconfig.Project{ID: "fixture", Workdir: t.TempDir(), Weight: 1},
			Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
		}, project.Dependencies{WorkAttempts: attempts, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tracked.Close() })
		if err := tracked.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-lookedUp
		registry := project.NewRegistry()
		mustSetProject(t, registry, tracked)
		gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{{ID: "fixture"}})
		if err != nil {
			t.Fatal(err)
		}
		controller := NewShutdownController()
		executed := false
		restart := NewProcessRestartRequest(func(binary string) error {
			if binary != "verified-controller" {
				t.Errorf("replacement binary = %q", binary)
			}
			executed = true
			return nil
		})
		t.Cleanup(controller.activate())
		updater, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{
			CheckInterval: time.Minute,
			RunningBuild:  runnerauth.BuildEvidence{Version: "1.0.0"},
			Updater: urgentRuntimeUpdater{apply: func(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error) {
				return detentupdate.Status{Action: detentupdate.ActionUpdated, LatestVersion: "1.0.1", Binary: "verified-controller"}, nil
			}},
			ReserveIdle: func(ctx context.Context) (func(), bool) { return runtimeUpdateIdleReservation(ctx, registry, gate) },
			ReserveDrain: func(context.Context) (func(), error) {
				t.Error("idle adoption waited for a drain")
				return nil, errors.New("unexpected drain")
			},
			RequestRestart: func(binary string) bool {
				accepted, err := requestRuntimeUpdateRestart(t.Context(), controller, restart, binary, func(ctx context.Context) (func(), bool) {
					return runtimeUpdateIdleReservation(ctx, registry, gate)
				})
				if err != nil {
					t.Error(err)
				}
				return accepted
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := updater.ApplyRelease(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if !executed || updater.Status().State != "restart_requested" {
			t.Fatalf("pending maintenance blocked adoption: executed=%t status=%+v", executed, updater.Status())
		}
	})
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
		if err := runtimeStore.RecordWorkAttemptHeartbeat(t.Context(), store.WorkAttemptHeartbeat{AttemptID: attemptID, HeartbeatAt: now, LeaseExpiresAt: now.Add(time.Hour), Phase: "completion_deferred", WorkerMetadataJSON: string(metadata)}); err != nil {
			t.Fatal(err)
		}
		cfg := workflowconfig.Default()
		cfg.Tracker.Kind = workflowconfig.TrackerHubNative
		tracked, err := projectfixture.New(project.Config{Project: globalconfig.Project{ID: "fixture", Workdir: t.TempDir(), Weight: 1}, Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."}}, project.Dependencies{WorkAttempts: runtimeStore})
		if err != nil {
			t.Fatal(err)
		}
		if err := tracked.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := tracked.Stop(ctx); err != nil && !errors.Is(err, project.ErrNotRunning) {
				t.Error(err)
			}
		})
		registry := project.NewRegistry()
		if err := registry.Set(tracked); err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		var state orchestrator.State
		testenv.WaitUntil(t, ctx, func() error {
			state, err = tracked.Orchestrator().State(ctx)
			if err != nil {
				return err
			}
			if state.UnsettledWork() == 0 {
				return errors.New("completion owner was not restored")
			}
			return nil
		})
		if err != nil || len(state.Snapshot(now).Running) != 0 || state.UnsettledWork() != 1 {
			t.Fatalf("completion owner unavailable: unsettled=%d err=%v", state.UnsettledWork(), err)
		}
		if runtimeUpdateIdle(t.Context(), registry) {
			t.Fatal("updater treated pending completion as idle")
		}
		cacheRoot := t.TempDir()
		cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheRoot, "README"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(cacheRoot, "00"), 0o700); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(cacheRoot, "00", strings.Repeat("a", 64)+"-d")
		if err := os.WriteFile(output, make([]byte, 2000), 0o600); err != nil {
			t.Fatal(err)
		}
		candidate := scheduler.ProjectCandidate{ID: "fixture"}
		gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{candidate})
		if err != nil {
			t.Fatal(err)
		}
		managed := &runnerBuildCache{cache: cache, root: cacheRoot}
		if err := managed.trim(ctx, toolcache.Policy{MaxBytes: 1000}, now); err != nil {
			t.Fatalf("deferred publication blocked quiescent cache reclamation: %v", err)
		}
		if report := managed.observe(); report == nil || report.SizeEvictedBytes != 2000 || report.RetainedBytes > 1000 {
			t.Fatalf("quiescent cache did not reclaim to budget during publication hold: %+v", report)
		}
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reclaimed cache output still exists: %v", err)
		}
		state, err = tracked.Orchestrator().State(ctx)
		if err != nil || state.UnsettledWork() != 1 {
			t.Fatalf("cache maintenance altered completion custody: unsettled=%d err=%v", state.UnsettledWork(), err)
		}
		slot, acquired, err := gate.TryAcquire(ctx, candidate, scheduler.SlotRequest{State: "Todo"}, now)
		if err != nil || !acquired {
			t.Fatalf("dispatch remained paused after cache reclamation: acquired=%t err=%v", acquired, err)
		}
		if err := gate.Release(slot); err != nil {
			t.Fatal(err)
		}
	})
}

type pendingAdmissionMaintenanceStore struct {
	store.Store
	lookedUp chan struct{}
}

func (s *pendingAdmissionMaintenanceStore) ListLocalAdmittedIssueIDs(context.Context, string) ([]string, error) {
	select {
	case s.lookedUp <- struct{}{}:
	default:
	}
	return nil, errors.New("store projections pending maintenance")
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

func TestUpdateRestartExecFailurePreservesClaiming(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		busy          bool
		operatorDrain bool
		supervised    bool
	}{
		{name: "failed process replacement resumes claims"},
		{name: "busy process is never replaced", busy: true},
		{name: "operator drain retains ownership", operatorDrain: true},
		{name: "failed busy controller startup resumes claims", busy: true, supervised: true},
		{name: "supervised adoption preserves operator drain", busy: true, supervised: true, operatorDrain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}}}, []scheduler.ProjectCandidate{{ID: "fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			controller := NewShutdownController()
			t.Cleanup(controller.activate())
			if test.operatorDrain {
				controller.RequestDrain()
			}
			native, _, _, workflow, create, _ := localWorkerHub(t)
			native.SetClaimAdmission(gate.ReserveDispatch)
			next := create("Next independent work")
			claimRequest := orchestrator.SchedulingRequest{ProjectID: "local", Policy: workflow.Config.Policy, WorkflowStates: []string{"Todo"}, CandidateLimit: 1}
			execErr := errors.New("process replacement failed")
			calls := 0
			restart := NewProcessRestartRequest(func(binary string) error {
				calls++
				if binary != "verified-controller" {
					t.Errorf("replacement binary = %q", binary)
				}
				if release, admitted := gate.ReserveDispatch(); admitted {
					release()
					t.Error("new claim admitted during process replacement")
				}
				if issues, err := native.FetchCandidateIssues(t.Context(), claimRequest); err != nil || len(issues) != 0 {
					t.Errorf("native claiming during process replacement: %+v %v", issues, err)
				}
				return execErr
			})
			if test.supervised {
				restart.control = &controllerConnection{}
				restart.reserve = gate.PauseDispatch
			}
			accepted, err := requestRuntimeUpdateRestart(t.Context(), controller, restart, "verified-controller", func(context.Context) (func(), bool) {
				return gate.PauseDispatch(), !test.busy
			})
			wantExec := (!test.busy || test.supervised) && !test.operatorDrain
			if accepted || errors.Is(err, execErr) != wantExec || (calls == 1) != wantExec {
				t.Fatalf("replacement accepted=%t err=%v calls=%d, want failure with execution=%t", accepted, err, calls, wantExec)
			}
			if !controller.Active() || controller.updateRestart.Load() || controller.shutdownRequested.Load() != test.operatorDrain || restart.Binary() != "" {
				t.Fatal("failed replacement changed the old controller lifecycle")
			}
			release, admitted := gate.ReserveDispatch()
			if !admitted {
				t.Fatal("failed replacement left native claiming paused")
			}
			release()
			if !test.operatorDrain {
				issues, err := native.FetchCandidateIssues(t.Context(), claimRequest)
				if err != nil || len(issues) != 1 || issues[0].ID != next.ID {
					t.Fatalf("old controller did not resume native claiming after exec failure: %+v %v", issues, err)
				}
			}
			select {
			case request := <-controller.Requests():
				if !test.operatorDrain || request != ShutdownRequestDrain {
					t.Fatalf("unexpected shutdown after exec failure: %v", request)
				}
			default:
				if test.operatorDrain {
					t.Fatal("failed replacement lost the operator drain")
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
			cfg.Tracker.Kind = workflowconfig.TrackerHubNative
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

func TestExceptionalUpdateDrainStopsNativeClaims(t *testing.T) {
	for _, outcome := range []string{"replacement", "preparation failure", "refused", "restart declined", "operator drain", "drain canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			updateCtx, cancelUpdate := context.WithCancel(ctx)
			defer cancelUpdate()
			var claims atomic.Int64
			native, _, connection, workflow, create, _ := localWorkerHub(t, func(r *http.Request) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claims") {
					claims.Add(1)
				}
			})
			activeIssue := create("Preserve active source")
			request := orchestrator.SchedulingRequest{ProjectID: "local", Policy: workflow.Config.Policy, WorkflowStates: []string{"Todo"}, CandidateLimit: 1}
			issues, err := native.FetchCandidateIssues(ctx, request)
			if err != nil || len(issues) != 1 || issues[0].ID != activeIssue.ID {
				t.Fatalf("initial fenced claim: %+v %v", issues, err)
			}
			nextIssue := create("Next independent work")
			candidates := []scheduler.ProjectCandidate{{ID: "local"}}
			pools := []scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 2}}}
			gate, err := scheduler.NewPoolRegistry(pools, candidates)
			if err != nil {
				t.Fatal(err)
			}
			native.SetClaimAdmission(gate.ReserveDispatch)
			slot, acquired, err := gate.TryAcquire(ctx, candidates[0], scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("active slot: %t %v", acquired, err)
			}
			artifacts, publish, _ := localWorkerArtifacts(t)
			publish("1.0.0")
			active, err := artifacts.Acquire(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := active.Release(); err != nil {
					t.Error(err)
				}
			}()
			source := filepath.Join(t.TempDir(), "source.go")
			if err := os.WriteFile(source, []byte("package example\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			publish("1.0.1")
			waiting := make(chan func(), 1)
			completed := make(chan struct{})
			finished := make(chan *runnerauth.UpdateObservation, 1)
			done := make(chan struct{})
			var applies, restarts int
			running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: time.Now()}
			updater, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{
				WorkerArtifacts: artifacts, WorkerPreflight: func(context.Context, string) error { return detentupdate.ErrWorkerProtocolIncompatible },
				Enabled: true, CheckInterval: time.Hour, StatePath: filepath.Join(t.TempDir(), "update.json"), RunningBuild: running,
				Updater: urgentRuntimeUpdater{apply: func(context.Context, detentupdate.ApplyOptions) (detentupdate.Status, error) {
					applies++
					if outcome == "preparation failure" || outcome == "operator drain" {
						return detentupdate.Status{FailureReason: "preflight failed"}, errors.New("preflight failed")
					}
					if outcome == "refused" {
						return detentupdate.Status{Action: detentupdate.ActionRefused}, detentupdate.ErrRefused
					}
					return detentupdate.Status{Action: detentupdate.ActionUpdated, LatestVersion: "1.0.1", Binary: "verified-controller"}, nil
				}},
				ReserveDrain: func(ctx context.Context) (func(), error) {
					release := gate.PauseDispatch()
					waiting <- release
					select {
					case <-completed:
						return release, nil
					case <-ctx.Done():
						release()
						return nil, ctx.Err()
					}
				},
				RequestRestart: func(string) bool { restarts++; return outcome != "restart declined" },
			})
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(done)
				finished <- updater.EnrolledUpdate(updateCtx, running, &runnerauth.UpdateRequest{RequestedAt: time.Now(), ID: "incompatible", Service: "detent", Version: "1.0.1", Release: true, Urgent: true})
			}()
			defer func() { cancel(); <-done }()
			releaseUpdate := <-waiting
			defer releaseUpdate()
			observation := updater.EnrolledUpdate(ctx, running, nil)
			if observation.Receipt == nil || observation.Receipt.Status != "draining" || observation.Receipt.DrainReason != detentupdate.ErrWorkerProtocolIncompatible.Error() {
				t.Fatalf("active drain receipt: %+v", observation)
			}
			before := claims.Load()
			checkPaused := func() {
				t.Helper()
				issues, err := native.FetchCandidateIssues(ctx, request)
				if err != nil || len(issues) != 0 || claims.Load() != before {
					t.Fatalf("drain requested new fenced work: claims=%d want=%d issues=%+v err=%v", claims.Load(), before, issues, err)
				}
			}
			checkPaused()
			pools = append(pools, scheduler.PoolConfig{Name: "other", Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 1}})
			candidates = append(candidates, scheduler.ProjectCandidate{ID: "other", Pool: "other"})
			if err := gate.Reconfigure(pools, candidates); err != nil {
				t.Fatal(err)
			}
			gate.SetProjects(candidates)
			checkPaused()
			if _, err := native.RenewClaim(ctx, activeIssue.ID, time.Now()); err != nil {
				t.Fatalf("drain lost active lease: %v", err)
			}
			if _, err := os.Stat(active.Status.Binary); err != nil {
				t.Fatalf("drain lost active artifact: %v", err)
			}
			if data, err := os.ReadFile(source); err != nil || string(data) != "package example\n" {
				t.Fatalf("drain lost source: %q %v", data, err)
			}
			if outcome == "drain canceled" {
				cancelUpdate()
				observation = <-finished
				if observation.Receipt.Status != "refused" || updater.Status().State != "pending_idle" || applies != 0 || restarts != 0 {
					t.Fatalf("canceled drain did not release active work: %+v applies/restarts=%d/%d", observation, applies, restarts)
				}
				if _, err := native.RenewClaim(ctx, activeIssue.ID, time.Now()); err != nil {
					t.Fatalf("failed drain lost active lease: %v", err)
				}
			}
			if err := connection.UpdateIssueState(ctx, activeIssue.ID, "Done"); err != nil {
				t.Fatal(err)
			}
			if err := native.ReleaseClaim(ctx, activeIssue.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			if err := gate.Release(slot); err != nil {
				t.Fatal(err)
			}
			releaseOperator := func() {}
			if outcome == "operator drain" {
				releaseOperator = gate.PauseDispatch()
			}
			defer releaseOperator()
			wantApplies := 0
			if outcome != "drain canceled" {
				close(completed)
				observation = <-finished
				wantApplies = 1
			}
			if applies != wantApplies {
				t.Fatalf("apply calls=%d", applies)
			}
			if outcome == "replacement" {
				if restarts != 1 || observation.Receipt.Status != "restart_requested" {
					t.Fatalf("replacement: %+v restarts=%d", observation, restarts)
				}
				checkPaused()
				replacementGate, err := scheduler.NewPoolRegistry(pools, candidates)
				if err != nil {
					t.Fatal(err)
				}
				native.SetClaimAdmission(replacementGate.ReserveDispatch)
			} else if observation.Receipt.Status == "draining" || observation.Receipt.Status == "restart_requested" {
				t.Fatalf("failed update retained drain receipt: %+v", observation)
			}
			if outcome == "operator drain" {
				checkPaused()
				releaseOperator()
			}
			issues, err = native.FetchCandidateIssues(ctx, request)
			if err != nil || len(issues) != 1 || issues[0].ID != nextIssue.ID || claims.Load() != before+1 {
				t.Fatalf("admission did not resume exactly once: %+v claims=%d %v", issues, claims.Load(), err)
			}
			if err := native.ReleaseClaim(ctx, issues[0].ID, "completed"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
