package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestHostCacheUsesExistingReaperSweep(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name     string
		last     time.Time
		want     int
		targeted bool
	}{
		{name: "startup", want: 1},
		{name: "due", last: now.Add(-2 * time.Hour), want: 1},
		{name: "not due", last: now, want: 1},
		{name: "targeted cleanup cannot postpone full sweep", last: now, targeted: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			at := now
			cfg := Config{WorkspaceCleanupSweepInterval: time.Hour, WorkspaceCleanupIdleTTL: time.Hour, HostCache: toolcache.Policy{MaxAge: time.Hour, MaxBytes: 12 << 30}}
			tracker := &runningStateConnector{}
			reaper := &cleanupSweepReaper{}
			orch := &Orchestrator{cfg: cfg, connector: tracker, reaper: reaper, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(_ context.Context, p toolcache.Policy, swept time.Time) error {
				calls++
				if p != cfg.HostCache || swept != at {
					t.Errorf("policy=%+v at=%v", p, swept)
				}
				return nil
			}}
			state := newState(cfg)
			state.LastWorkspaceCleanupAt = tt.last
			passes := []int{tt.want}
			if tt.targeted {
				idleSince := now.Add(-2 * time.Hour)
				issue := connector.Issue{ID: "blocked", Identifier: "repo#1", State: "Blocked", StageUpdatedAt: &idleSince}
				tracker.issues = []connector.Issue{issue}
				state.Blocked[issue.ID] = Blocked{Issue: issue}
				passes = []int{1, 2, 3, 4, 5, 6}
			}
			for i, want := range passes {
				if tt.targeted {
					at = now.Add(time.Duration(i+1) * 20 * time.Minute)
				}
				orch.startWorkspaceCleanup(t.Context(), &state, at)
				finishTestWorkspaceCleanup(t, orch, &state)
				if calls != want {
					t.Fatalf("pass %d calls=%d want=%d", i, calls, want)
				}
				if tt.targeted && len(reaper.issues) != i+1 {
					t.Fatalf("pass %d targeted reaps=%d want=%d", i, len(reaper.issues), i+1)
				}
			}
		})
	}
}

type stalledCacheCleanupConnector struct {
	runningStateConnector
	entered chan struct{}
	release chan struct{}
}

func (c *stalledCacheCleanupConnector) FetchIssueStatesByIDs(ctx context.Context, _ []string) ([]connector.Issue, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestHostCacheReclaimsBeforeTrackerCleanup(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart after exit", true: "consumer outlives worker"}[busy], func(t *testing.T) {
			root := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0700); err != nil {
				t.Fatal(err)
			}
			entry := filepath.Join(root, "00", strings.Repeat("a", 64)+"-d")
			if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
				t.Fatal(err)
			}
			release := func() error { return nil }
			pin := filepath.Join(t.TempDir(), "active-output")
			if busy {
				_, release, err = cache.Use(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Link(entry, pin); err != nil {
					t.Fatal(err)
				}
			}
			defer release()
			tracker := &stalledCacheCleanupConnector{entered: make(chan struct{}, 1), release: make(chan struct{})}
			cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}, WorkspaceCleanupSweepInterval: time.Hour, HostCache: toolcache.Policy{MaxBytes: 1024}})
			var calls atomic.Int32
			o := &Orchestrator{cfg: cfg, connector: tracker, reaper: &cleanupSweepReaper{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(ctx context.Context, p toolcache.Policy, now time.Time) error {
				calls.Add(1)
				_, err := cache.Maintain(ctx, p, now)
				return err
			}}
			defer o.stopWorkspaceCleanup()
			defer close(tracker.release)
			state := newState(cfg)
			state.Completed["exited"] = Completed{Issue: connector.Issue{ID: "exited", Identifier: "owner/repository#1", State: "Done"}}
			now := time.Now()
			state.LastWorkspaceCleanupAt = now
			o.startWorkspaceCleanup(t.Context(), &state, now)
			<-tracker.entered
			if calls.Load() != 1 {
				t.Fatal("idle cache maintenance waited for tracker cleanup")
			}
			if _, err := os.Stat(entry); os.IsNotExist(err) == busy {
				t.Fatalf("entry stat=%v busy=%v", err, busy)
			}
			measuredFree := func() uint64 {
				info, err := os.Stat(entry)
				if os.IsNotExist(err) {
					return 2500
				}
				if err != nil {
					t.Fatal(err)
				}
				return 2500 - uint64(info.Size())
			}
			reserve := uint64(toolcache.Policy{}.NormalizedForCapacity(10000).MaxBytes)
			checked := false
			o.scheduling = &storageCleanupScheduling{constraint: func() string {
				if measuredFree() < reserve {
					return "below reserve"
				}
				return ""
			}, check: func() {
				checked = true
				if (measuredFree() >= reserve) == busy {
					t.Fatalf("free=%d reserve=%d busy=%v", measuredFree(), reserve, busy)
				}
			}}
			if _, err := o.fetchCandidateIssuesForTick(t.Context(), &state); err != nil {
				t.Fatal(err)
			}
			if !checked {
				t.Fatal("tracker cleanup blocked capacity observation")
			}
			o.startWorkspaceCleanup(t.Context(), &state, now)
			if calls.Load() != 1 {
				t.Fatal("second cleanup owner started")
			}
			if busy {
				_, secondRelease, err := cache.Use(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := secondRelease(); err != nil {
					t.Fatal(err)
				}
			}
			o.workspaceCleanupCancel()
			finishTestWorkspaceCleanup(t, o, &state)
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if busy {
				if err := os.Remove(pin); err != nil {
					t.Fatal(err)
				}
			}
			o.startWorkspaceCleanup(t.Context(), &state, now.Add(time.Second))
			<-tracker.entered
			if calls.Load() != 2 {
				t.Fatal("next lifecycle opportunity did not resume cache maintenance")
			}
			if _, err := os.Stat(entry); !os.IsNotExist(err) {
				t.Fatalf("idle cache not reclaimed: %v", err)
			}
		})
	}
}

func TestHostCacheMaintenanceBudget(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll budget", true: "shutdown"}[stop], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := normalizeConfig(Config{PollInterval: time.Second})
				maintenanceErrors := make(chan error, 1)
				o := &Orchestrator{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(ctx context.Context, _ toolcache.Policy, _ time.Time) error {
					<-ctx.Done()
					err := ctx.Err()
					maintenanceErrors <- err
					return err
				}}
				defer o.stopWorkspaceCleanup()
				state := newState(cfg)
				now := time.Now()
				o.startWorkspaceCleanup(t.Context(), &state, now)
				synctest.Wait()
				o.reclaimWorkspaceStorage(t.Context(), &state, now)
				select {
				case err := <-maintenanceErrors:
					t.Fatalf("admission waited for maintenance budget: %v", err)
				default:
				}
				if stop {
					o.stopWorkspaceCleanup()
				}
				result := <-o.workspaceCleanupResults
				o.finishWorkspaceCleanup(&state, result)
				o.workspaceCleanupWG.Wait()
				want := context.DeadlineExceeded
				if stop {
					want = context.Canceled
				}
				maintenanceErr := <-maintenanceErrors
				if !errors.Is(maintenanceErr, want) {
					t.Fatalf("maintenance error=%v want=%v", maintenanceErr, want)
				}
			})
		})
	}
}

func TestHostCacheStartsBeforeCompletionTrackerRead(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "success"}, {name: "failure", err: errors.New("worker failed")}, {name: "cancellation", err: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				tracker := &stalledCacheCleanupConnector{entered: make(chan struct{}, 1), release: make(chan struct{})}
				var calls atomic.Int32
				cfg := normalizeConfig(Config{})
				o := &Orchestrator{cfg: cfg, connector: tracker, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), trimHostCache: func(context.Context, toolcache.Policy, time.Time) error { calls.Add(1); return nil }}
				state := newState(cfg)
				issue := connector.Issue{ID: "exited", Identifier: "owner/repository#1", State: "In Progress"}
				state.Running[issue.ID] = Running{Issue: issue, Generation: 1}
				done := make(chan struct{})
				go func() {
					defer close(done)
					o.handleRunResult(ctx, &state, runpkg.Completion{IssueID: issue.ID, Request: RunRequest{Generation: 1}, CompletedAt: time.Now(), Err: tt.err})
				}()
				defer func() { cancel(); close(tracker.release); <-done; o.stopWorkspaceCleanup() }()
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatal("completion tracker read postponed local maintenance")
				}
				select {
				case <-done:
					t.Fatal("completion did not encounter stalled tracker")
				default:
				}
			})
		})
	}
}
