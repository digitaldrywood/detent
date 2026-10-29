package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type inventoryCleanupReaper struct {
	cleanupSweepReaper
	inventory  []connector.Issue
	cursors    []string
	reconciles int
}

func (r *inventoryCleanupReaper) ReconcileWorkspaces(context.Context, []connector.Issue) (WorkspaceReconcileResult, error) {
	r.reconciles++
	return WorkspaceReconcileResult{}, nil
}

func (r *inventoryCleanupReaper) WorkspaceCleanupIssues(_ context.Context, cursor string, _ int) ([]connector.Issue, string, error) {
	r.cursors = append(r.cursors, cursor)
	return r.inventory, "next", nil
}

func TestWorkspaceCleanupRetriesLocalInventory(t *testing.T) {
	cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}, WorkspaceCleanupSweepInterval: time.Hour})
	issue := connector.Issue{ID: "wi_closed", Identifier: "prj_example#1", State: "Done", Closed: true}
	tracker := &runningStateConnector{issues: []connector.Issue{issue}, err: errors.New("Hub request failed with status 502")}
	reaper := &inventoryCleanupReaper{inventory: []connector.Issue{issue, issue}}
	o := &Orchestrator{cfg: cfg, connector: tracker, reaper: reaper, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	state := newState(cfg)
	now := time.Now()
	state.LastWorkspaceCleanupAt = now
	state.ReapedWorkspaces[issue.ID] = now.Add(-time.Hour)
	o.startWorkspaceCleanup(t.Context(), &state, now)
	finishTestWorkspaceCleanup(t, o, &state)
	if len(reaper.issues) != 0 || state.workspaceCleanupInventoryCursor != "" {
		t.Fatalf("failed fetch advanced cleanup: issues=%v cursor=%q", reaper.issues, state.workspaceCleanupInventoryCursor)
	}
	tracker.err = nil
	o.startWorkspaceCleanup(t.Context(), &state, now.Add(time.Second))
	finishTestWorkspaceCleanup(t, o, &state)
	if len(reaper.issues) != 1 || reaper.issues[0].ID != issue.ID || !slices.Equal(tracker.requestedIDs, []string{issue.ID}) {
		t.Fatalf("later tick did not clean the local backlog: reaped=%v requested=%v", reaper.issues, tracker.requestedIDs)
	}
	if !slices.Equal(reaper.cursors, []string{"", ""}) || state.workspaceCleanupInventoryCursor != "next" {
		t.Fatalf("inventory did not retry the same page: cursors=%v next=%q", reaper.cursors, state.workspaceCleanupInventoryCursor)
	}
	if reaper.reconciles != 2 {
		t.Fatalf("local inventory reconciliation skipped a tick: %d", reaper.reconciles)
	}
}

type storageCleanupScheduling struct {
	*hubSchedulingSource
	check      func()
	constraint func() string
}

func (s *storageCleanupScheduling) WorkspaceStorageConstraint(context.Context) string {
	return s.constraint()
}

func (s *storageCleanupScheduling) FetchCandidateIssues(context.Context, SchedulingRequest) ([]connector.Issue, error) {
	s.check()
	return nil, nil
}

func TestStoragePressureReclaimsBeforeClaim(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		for _, freed := range []bool{false, true} {
			t.Run(fmt.Sprintf("in_flight=%v/freed=%v", inFlight, freed), func(t *testing.T) {
				cfg := normalizeConfig(Config{ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, WorkspaceCleanupSweepInterval: time.Hour})
				issue := connector.Issue{ID: "closed", Identifier: "owner/repository#1", State: "Done", Closed: true}
				tracker := &runningStateConnector{issuesByState: []connector.Issue{issue}}
				reaper := &cleanupSweepReaper{}
				var logs bytes.Buffer
				projections := &retentionProjectionStore{}
				o := &Orchestrator{cfg: cfg, connector: tracker, reaper: reaper, workAttempts: projections, logger: slog.New(slog.NewTextHandler(&logs, nil))}
				state := newState(cfg)
				now := time.Now()
				state.LastWorkspaceCleanupAt = now
				if inFlight {
					o.startWorkspaceCleanupPass(t.Context(), &state, now, true)
				}
				checked := false
				observations := 0
				o.scheduling = &storageCleanupScheduling{constraint: func() string {
					observations++
					if observations > 1 && freed {
						return ""
					}
					return "below reserve"
				}, check: func() {
					checked = true
					if len(reaper.issues) != 1 || reaper.issues[0].ID != issue.ID || o.workspaceCleanupCancel != nil {
						t.Fatalf("claim checked capacity before reclamation: reaped=%v active=%v", reaper.issues, o.workspaceCleanupCancel != nil)
					}
					if strings.Contains(logs.String(), "runner host storage limits capacity") == freed {
						t.Fatalf("capacity log did not use storage after cleanup: %s", logs.String())
					}
				}}
				if _, err := o.fetchCandidateIssuesForTick(t.Context(), &state); err != nil {
					t.Fatal(err)
				}
				o.stopWorkspaceCleanup()
				if !checked {
					t.Fatal("claim path was not reached")
				}
				if projections.calls == 0 {
					t.Fatal("cleanup pass skipped the store projection backfill")
				}
			})
		}
	}
}

func TestActiveWorkspaceIssuesRetainsRunningOwnership(t *testing.T) {
	cfg := normalizeConfig(Config{TerminalStates: []string{"Done"}})
	state := newState(cfg)
	issue := connector.Issue{ID: "wi_running", Identifier: "prj_example#1", State: "Done", Closed: true}
	state.BoardIssues = []connector.Issue{issue}
	state.Running[issue.ID] = Running{Issue: issue}
	active := activeWorkspaceIssues(&state, cfg.TerminalStates)
	if len(active) != 1 || active[0].Metadata["workspace_cleanup_running"] != "true" {
		t.Fatalf("running lease not preserved for workspace cleanup: %+v", active)
	}
	if state.Running[issue.ID].Issue.Metadata["workspace_cleanup_running"] != "" {
		t.Fatal("cleanup mutated the running issue")
	}
}

func TestCleanupIssueOrder(t *testing.T) {
	t.Parallel()
	unordered := []connector.Issue{{ID: "c"}, {ID: "a"}, {ID: "b"}}
	tests := []struct {
		name   string
		issues []connector.Issue
		cursor string
		want   []string
	}{
		{name: "nil issues"},
		{name: "empty issues", issues: []connector.Issue{}, cursor: "b"},
		{name: "single issue", issues: []connector.Issue{{ID: "b"}}, cursor: "b", want: []string{"b"}},
		{name: "before first", issues: unordered, cursor: "", want: []string{"a", "b", "c"}},
		{name: "at first", issues: unordered, cursor: "a", want: []string{"b", "c", "a"}},
		{name: "between issues", issues: unordered, cursor: "bb", want: []string{"c", "a", "b"}},
		{name: "at last", issues: unordered, cursor: "c", want: []string{"a", "b", "c"}},
		{name: "after last", issues: unordered, cursor: "z", want: []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := slices.Clone(tt.issues)
			before := slices.Clone(issues)
			ordered := cleanupIssueOrder(issues, tt.cursor)
			var ids []string
			for _, issue := range ordered {
				ids = append(ids, issue.ID)
			}
			if !slices.Equal(ids, tt.want) {
				t.Fatalf("cleanupIssueOrder() IDs = %v, want %v", ids, tt.want)
			}
			if !reflect.DeepEqual(issues, before) {
				t.Fatal("cleanupIssueOrder() changed the input")
			}
			if len(ordered) > 0 {
				ordered[0].ID = "changed"
				if !reflect.DeepEqual(issues, before) {
					t.Fatal("cleanupIssueOrder() result aliases the input")
				}
			}
		})
	}
}

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

func TestWorkspaceCleanupNeverAppliesTrackerObservations(t *testing.T) {
	for _, newerRefresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("newer_refresh=%v", newerRefresh), func(t *testing.T) {
			before := newState(Config{})
			before.LastRefreshAt = time.Now()
			before.Running["issue"] = Running{Issue: connector.Issue{ID: "issue", State: "In Progress"}, Generation: 1}
			after := before.clone()
			terminal := after.Running["issue"]
			terminal.Issue.State = "Done"
			terminal.CompletionLane = "Done"
			after.Running["issue"] = terminal
			current := before.clone()
			if newerRefresh {
				// A newer refresh saw the terminal lane and then its reversal. State
				// equality cannot distinguish this from the original active snapshot.
				current.Running["issue"] = terminal
				current.LastRefreshAt = before.LastRefreshAt.Add(time.Minute)
				active := current.Running["issue"]
				active.Issue.State = "In Progress"
				active.CompletionLane = ""
				current.Running["issue"] = active
				current.LastRefreshAt = before.LastRefreshAt.Add(2 * time.Minute)
			}
			o := &Orchestrator{workspaceCleanupCancel: func() {}}
			o.finishWorkspaceCleanup(&current, workspaceCleanupResult{before: before, after: after})
			if got := current.Running["issue"]; got.Issue.State != "In Progress" || got.CompletionLane != "" {
				t.Fatalf("cleanup replaced authoritative running observation: %+v", got)
			}
		})
	}
}
