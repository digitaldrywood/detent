package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
)

const parallelOwnersHelper = "DETENT_PGTEST_PARALLEL_OWNERS"

const (
	fixtureParents   = 4
	parentChildren   = 2
	unrelatedWaiters = 2
)

type namedOwner struct {
	*testing.T
	name string
}

func (o namedOwner) Name() string { return o.name }

func TestFixtureParentsWithParallelChildrenFinish(t *testing.T) {
	if os.Getenv(parallelOwnersHelper) != "" {
		runFixtureParentsWithParallelChildren(t)
		return
	}
	processTemplate.Database(t)
	for _, tc := range []struct {
		name      string
		selection string
	}{
		{name: "binary shared cluster", selection: "binary"},
		{name: "attached server", selection: serverSelection(processTemplate.server)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFixtureParentsWithParallelChildrenFinish$", "-test.timeout=45s", "-test.v")
			cmd.Env = append(os.Environ(), parallelOwnersHelper+"=1", Environment+"="+tc.selection, "GOCOVERDIR="+t.TempDir(), "TMPDIR="+t.TempDir())
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fixture parents with parallel children and unrelated waiters did not finish: %v\n%s", err, output)
			}
		})
	}
}

type fixtureParent struct {
	database string
	db       *sql.DB
	ctx      context.Context
	started  atomic.Int32
	finished atomic.Int32
}

func runFixtureParentsWithParallelChildren(t *testing.T) {
	runSlots := make(chan struct{}, unrelatedWaiters)
	parents := make([]*fixtureParent, fixtureParents)
	var holding, waitersRunning, waitersDone, parentsDone sync.WaitGroup
	holding.Add(fixtureParents)
	waitersRunning.Add(unrelatedWaiters)
	childrenMayRun := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(childrenMayRun) }) })
	for i := range parents {
		parent := &fixtureParent{}
		parents[i] = parent
		runSlots <- struct{}{}
		parentsDone.Go(func() {
			t.Run(fmt.Sprintf("parent %d", i), func(t *testing.T) {
				held := sync.OnceFunc(holding.Done)
				t.Cleanup(held)
				releaseSlot := sync.OnceFunc(func() { <-runSlots })
				t.Cleanup(releaseSlot)
				ctx, cancel := testContext(t)
				t.Cleanup(cancel)
				parent.ctx = ctx
				parent.db = processTemplate.DatabaseContext(ctx, namedOwner{T: t, name: fmt.Sprintf("TestParent%d", i)})
				if err := parent.db.QueryRowContext(ctx, "SELECT current_database()").Scan(&parent.database); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if started, finished := parent.started.Load(), parent.finished.Load(); started != parentChildren || finished != parentChildren {
						t.Errorf("parent %d fixture cleanup ran with %d children started and %d finished, want %d", i, started, finished, parentChildren)
					}
				})
				held()
				releaseSlot()
				var children sync.WaitGroup
				for child := range parentChildren {
					children.Go(func() {
						<-childrenMayRun
						runSlots <- struct{}{}
						defer func() { <-runSlots }()
						t.Run(fmt.Sprintf("child %d", child), func(t *testing.T) {
							parent.started.Add(1)
							useParentDatabase(t, parent, i, child)
							parent.finished.Add(1)
						})
					})
				}
				children.Wait()
			})
		})
	}
	holding.Wait()
	waiterDatabases := make([]string, unrelatedWaiters)
	for i := range unrelatedWaiters {
		waitersDone.Go(func() {
			runSlots <- struct{}{}
			defer func() { <-runSlots }()
			t.Run(fmt.Sprintf("unrelated waiter %d", i), func(t *testing.T) {
				waitersRunning.Done()
				ctx, cancel := testContext(t)
				defer cancel()
				db := processTemplate.DatabaseContext(ctx, namedOwner{T: t, name: fmt.Sprintf("TestWaiter%d", i)})
				if err := db.QueryRowContext(ctx, "SELECT current_database()").Scan(&waiterDatabases[i]); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
	waitersRunning.Wait()
	unblock.Do(func() { close(childrenMayRun) })
	waitersDone.Wait()
	parentsDone.Wait()
	if len(runSlots) != 0 {
		t.Fatalf("modeled run slots still held: %d", len(runSlots))
	}
	names := map[string]bool{}
	for i, parent := range parents {
		if started, finished := parent.started.Load(), parent.finished.Load(); started != parentChildren || finished != parentChildren {
			t.Fatalf("parent %d children started=%d finished=%d, want %d", i, started, finished, parentChildren)
		}
		names[parent.database] = true
	}
	for _, name := range waiterDatabases {
		names[name] = true
	}
	if len(names) != fixtureParents+unrelatedWaiters || names[""] {
		t.Fatalf("distinct fixture databases = %d, want %d", len(names), fixtureParents+unrelatedWaiters)
	}
	processTemplate.server.mu.Lock()
	for name := range names {
		if _, owned := processTemplate.server.owned[name]; owned {
			t.Errorf("fixture database %s is still owned after its test finished", name)
		}
	}
	processTemplate.server.mu.Unlock()
	for name := range names {
		requireDatabaseAbsent(t, name)
	}
	stats := processTemplate.server.admin.Stats()
	if stats.MaxOpenConnections != adminConnectionLimit || stats.InUse != 0 {
		t.Fatalf("administrative clone pool max=%d in use=%d, want max %d and none in use", stats.MaxOpenConnections, stats.InUse, adminConnectionLimit)
	}
}

func useParentDatabase(t *testing.T, parent *fixtureParent, index, child int) {
	t.Helper()
	ctx := parent.ctx
	var current string
	if err := parent.db.QueryRowContext(ctx, "SELECT current_database()").Scan(&current); err != nil {
		t.Fatalf("parent %d child %d lost its fixture database: %v", index, child, err)
	}
	if current != parent.database {
		t.Fatalf("parent %d child %d reached database %s, want %s", index, child, current, parent.database)
	}
	id := fmt.Sprintf("parent-%d-child-%d", index, child)
	if _, err := parent.db.ExecContext(ctx, "INSERT INTO records VALUES ('first', $1, 'private')", id); err != nil {
		t.Fatal(err)
	}
	var foreign int
	if err := parent.db.QueryRowContext(ctx, "SELECT count(*) FROM records WHERE id NOT LIKE $1", fmt.Sprintf("parent-%d-child-%%", index)).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if foreign != 0 {
		t.Fatalf("parent %d database holds %d rows written by other parents", index, foreign)
	}
}
