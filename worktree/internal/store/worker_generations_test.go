package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store/sqlc"
)

func newGenerationDatabase(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	database, err := migratedTestDatabase()
	if err != nil {
		t.Fatalf("build migrated store fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "detent.db")
	if err := os.WriteFile(path, database, 0o600); err != nil {
		t.Fatalf("write migrated store fixture: %v", err)
	}
	return path
}

func openGenerationStore(t *testing.T, path string, busyTimeout time.Duration) *sqliteStore {
	t.Helper()
	backend, err := Open(t.Context(), Config{Backend: BackendSQLite, Path: path, BusyTimeout: busyTimeout})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return backend.(*sqliteStore)
}

func selfProcessStart(t *testing.T) string {
	t.Helper()
	start, err := procstart.Identity(os.Getpid())
	if err != nil {
		t.Fatalf("procstart.Identity(self) error = %v", err)
	}
	return start
}

func exitedProcess(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	start, err := procstart.Identity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("procstart.Identity(child) error = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = cmd.Wait()
	return cmd.Process.Pid, start
}

func registerGeneration(t *testing.T, s *sqliteStore, pid int, start string) int64 {
	t.Helper()
	generation, err := s.StartWorkerGeneration(t.Context(), WorkerGenerationStart{PID: pid, ProcessStart: start, Version: "test", StartedAt: time.Now()})
	if err != nil {
		t.Fatalf("StartWorkerGeneration() error = %v", err)
	}
	return generation
}

func TestWorkerGenerationOwnerStamping(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		registered bool
	}{
		{name: "registered generation stamps rows", registered: true},
		{name: "unregistered store writes legacy rows"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := openGenerationStore(t, newGenerationDatabase(t), 0)
			var want int64
			if test.registered {
				want = registerGeneration(t, s, os.Getpid(), selfProcessStart(t))
				if want <= 0 || s.WorkerGeneration() != want {
					t.Fatalf("generation = %d, WorkerGeneration() = %d", want, s.WorkerGeneration())
				}
			}
			now := time.Now().UTC()
			attemptID, err := s.StartWorkAttempt(t.Context(), WorkAttemptStart{ProjectID: "detent", IssueID: "issue-1", WorkerType: "implement", StartedAt: now})
			if err != nil {
				t.Fatalf("StartWorkAttempt() error = %v", err)
			}
			sessionID, err := s.StartSession(t.Context(), SessionStart{ProjectID: "detent", IssueID: "issue-1", WorkAttemptID: attemptID, StartedAt: now})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}
			if err := s.UpdateSessionWorkerProcess(t.Context(), sessionID, WorkerProcessRegistration{WorkerProcessIdentity: WorkerProcessIdentity{PID: 4242, GroupID: 4242, StartedAt: now}}); err != nil {
				t.Fatalf("UpdateSessionWorkerProcess() error = %v", err)
			}
			for _, column := range []struct {
				table string
				id    int64
			}{{"work_attempts", attemptID}, {"codex_sessions", sessionID}} {
				var owner sql.NullInt64
				if err := s.db.QueryRowContext(t.Context(), "SELECT owner_generation FROM "+column.table+" WHERE id = ?", column.id).Scan(&owner); err != nil {
					t.Fatal(err)
				}
				if owner.Valid != test.registered || owner.Int64 != want {
					t.Fatalf("%s.owner_generation = %+v, want %d (valid=%t)", column.table, owner, want, test.registered)
				}
			}
			attempt, err := s.WorkAttempt(t.Context(), attemptID)
			if err != nil {
				t.Fatal(err)
			}
			processes, err := s.ListActiveWorkerProcesses(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if attempt.OwnerGeneration != want || len(processes) != 1 || processes[0].OwnerGeneration != want {
				t.Fatalf("attempt owner = %d, processes = %+v, want owner %d", attempt.OwnerGeneration, processes, want)
			}
		})
	}
}

func TestLiveWorkerGenerations(t *testing.T) {
	t.Parallel()
	path := newGenerationDatabase(t)
	observer := openGenerationStore(t, path, 0)
	self := selfProcessStart(t)
	observerGeneration := registerGeneration(t, observer, os.Getpid(), self)
	deadPID, deadStart := exitedProcess(t)

	rows := []struct {
		name     string
		pid      int
		start    string
		exited   bool
		wantLive bool
	}{
		{name: "live sibling", pid: os.Getpid(), start: self, wantLive: true},
		{name: "exited cleanly", pid: os.Getpid(), start: self, exited: true},
		{name: "dead pid", pid: deadPID, start: deadStart},
		{name: "pid reused", pid: os.Getpid(), start: deadStart},
	}
	generations := make(map[string]int64, len(rows))
	now := time.Now().UTC().Format(time.RFC3339)
	for _, row := range rows {
		generation, err := observer.queries.CreateWorkerGeneration(t.Context(), sqlc.CreateWorkerGenerationParams{
			Pid: int64(row.pid), ProcessStart: row.start, Version: "test", State: WorkerGenerationStateActive, StartedAt: now, UpdatedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if row.exited {
			if _, err := observer.queries.ExitWorkerGenerations(t.Context(), sqlc.ExitWorkerGenerationsParams{
				State: WorkerGenerationStateExited, ExitedAt: sql.NullString{String: now, Valid: true}, Generations: "[" + strconv.FormatInt(generation, 10) + "]",
			}); err != nil {
				t.Fatal(err)
			}
		}
		generations[row.name] = generation
	}

	live, err := observer.LiveWorkerGenerations(t.Context())
	if err != nil {
		t.Fatalf("LiveWorkerGenerations() error = %v", err)
	}
	foreign, err := observer.LiveForeignWorkerGenerations(t.Context())
	if err != nil {
		t.Fatalf("LiveForeignWorkerGenerations() error = %v", err)
	}
	if !slices.Contains(live, observerGeneration) || slices.Contains(foreign, observerGeneration) {
		t.Fatalf("observer generation %d: live=%v foreign=%v", observerGeneration, live, foreign)
	}
	for _, row := range rows {
		generation := generations[row.name]
		if got := slices.Contains(live, generation); got != row.wantLive {
			t.Errorf("%s: live = %t, want %t (live=%v)", row.name, got, row.wantLive, live)
		}
		if got := slices.Contains(foreign, generation); got != row.wantLive {
			t.Errorf("%s: foreign = %t, want %t (foreign=%v)", row.name, got, row.wantLive, foreign)
		}
	}

	next := openGenerationStore(t, path, 0)
	registerGeneration(t, next, os.Getpid(), self)
	unexited, err := next.queries.ListUnexitedWorkerGenerations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remaining := make([]int64, 0, len(unexited))
	for _, row := range unexited {
		remaining = append(remaining, row.Gen)
	}
	for _, row := range rows {
		if got := slices.Contains(remaining, generations[row.name]); got != row.wantLive {
			t.Errorf("%s: unexited after next allocation = %t, want %t", row.name, got, row.wantLive)
		}
	}
}

type recoveryOwners struct {
	current *sqliteStore
	foreign *sqliteStore
	now     time.Time
}

func newRecoveryOwners(t *testing.T) recoveryOwners {
	t.Helper()
	path := newGenerationDatabase(t)
	self := selfProcessStart(t)
	deadPID, deadStart := exitedProcess(t)
	now := time.Now().UTC().Truncate(time.Second)

	legacy := openGenerationStore(t, path, 0)
	dead := openGenerationStore(t, path, 0)
	registerGeneration(t, dead, deadPID, deadStart)
	reused := openGenerationStore(t, path, 0)
	registerGeneration(t, reused, os.Getpid(), deadStart)
	foreign := openGenerationStore(t, path, 0)
	registerGeneration(t, foreign, os.Getpid(), self)
	current := openGenerationStore(t, path, 0)
	registerGeneration(t, current, os.Getpid(), self)

	for owner, writer := range map[string]*sqliteStore{"legacy": legacy, "dead": dead, "reused": reused, "foreign": foreign, "current": current} {
		seedRecoveryRows(t, writer, owner, now)
	}
	return recoveryOwners{current: current, foreign: foreign, now: now}
}

func seedRecoveryRows(t *testing.T, s *sqliteStore, owner string, now time.Time) {
	t.Helper()
	ctx := t.Context()
	activeID, err := s.StartWorkAttempt(ctx, WorkAttemptStart{
		ProjectID: "detent", IssueID: "active-" + owner, WorkerType: "implement",
		StartedAt: now.Add(-time.Hour), LeaseExpiresAt: now.Add(-time.Minute),
		WorkerMetadataJSON: `{"run_mode":"local"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartSession(ctx, SessionStart{
		ProjectID: "detent", IssueID: "active-" + owner, WorkAttemptID: activeID,
		StartedAt: now.Add(-time.Hour), ProviderThreadID: "thread-" + owner,
	}); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []struct {
		prefix string
		update string
	}{
		{"stop-", "UPDATE work_attempts SET status = 'terminal', completed_at = ?, terminal_state = 'operator_stopped', phase = 'operator_stop_pending' WHERE id = ?"},
		{"release-", "UPDATE work_attempts SET status = 'terminal', completed_at = ?, terminal_state = 'success', next_action = 'release capacity' WHERE id = ?"},
	} {
		id, err := s.StartWorkAttempt(ctx, WorkAttemptStart{ProjectID: "detent", IssueID: terminal.prefix + owner, WorkerType: "implement", StartedAt: now.Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, terminal.update, now.Add(-time.Minute).Format(time.RFC3339), id); err != nil {
			t.Fatal(err)
		}
	}
}

func attemptOwners(attempts []WorkAttempt, prefix string) []string {
	var owners []string
	for _, attempt := range attempts {
		if owner, ok := strings.CutPrefix(attempt.IssueID, prefix); ok {
			owners = append(owners, owner)
		}
	}
	slices.Sort(owners)
	return owners
}

func TestRecoveryQueriesSkipLiveForeignGenerations(t *testing.T) {
	t.Parallel()
	recoverable := []string{"current", "dead", "legacy", "reused"}
	everyone := []string{"current", "dead", "foreign", "legacy", "reused"}
	tests := []struct {
		name          string
		foreignExited bool
		want          []string
		query         func(context.Context, recoveryOwners) ([]string, error)
	}{
		{
			name: "orphaned agent sessions",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				sessions, err := owners.current.ListOrphanedAgentSessions(ctx, "detent")
				var got []string
				for _, session := range sessions {
					got = append(got, strings.TrimPrefix(session.IssueID, "active-"))
				}
				slices.Sort(got)
				return got, err
			},
		},
		{
			name: "expired lease timeout",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.TimeoutExpiredWorkAttempts(ctx, WorkAttemptTimeout{ProjectID: "detent", Now: owners.now})
				return attemptOwners(attempts, "active-"), err
			},
		},
		{
			name: "confirmed gone timeout",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				active, err := owners.current.ListActiveWorkAttempts(ctx, WorkAttemptQuery{})
				if err != nil {
					return nil, err
				}
				gone := make([]int64, 0, len(active))
				for _, attempt := range active {
					gone = append(gone, attempt.ID)
				}
				attempts, err := owners.current.TimeoutExpiredWorkAttempts(ctx, WorkAttemptTimeout{Now: owners.now.Add(-2 * time.Hour), ConfirmedGoneAttemptIDs: gone, TerminalState: WorkAttemptTerminalAbandoned, ErrorClass: "service_restart"})
				return attemptOwners(attempts, "active-"), err
			},
		},
		{
			name: "deferred completion candidates",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.ListActiveWorkAttempts(ctx, WorkAttemptQuery{ProjectID: "detent", ExcludeLiveForeignGenerations: true})
				return attemptOwners(attempts, "active-"), err
			},
		},
		{
			name: "unscoped active attempts keep every owner",
			want: everyone,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.ListActiveWorkAttempts(ctx, WorkAttemptQuery{ProjectID: "detent"})
				return attemptOwners(attempts, "active-"), err
			},
		},
		{
			name: "pending operator stops",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.ListPendingOperatorStops(ctx, "detent")
				return attemptOwners(attempts, "stop-"), err
			},
		},
		{
			name: "pending capacity releases",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.ListPendingWorkAttemptCapacityReleases(ctx, "detent")
				return attemptOwners(attempts, "release-"), err
			},
		},
		{
			name: "local admissions",
			want: recoverable,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				ids, err := owners.current.ListLocalAdmittedIssueIDs(ctx, "detent")
				var got []string
				for _, id := range ids {
					got = append(got, strings.TrimPrefix(id, "active-"))
				}
				slices.Sort(got)
				return got, err
			},
		},
		{
			name:          "exited foreign generation is recoverable",
			foreignExited: true,
			want:          everyone,
			query: func(ctx context.Context, owners recoveryOwners) ([]string, error) {
				attempts, err := owners.current.TimeoutExpiredWorkAttempts(ctx, WorkAttemptTimeout{ProjectID: "detent", Now: owners.now})
				return attemptOwners(attempts, "active-"), err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owners := newRecoveryOwners(t)
			if test.foreignExited {
				if err := owners.foreign.ExitWorkerGeneration(t.Context(), owners.now); err != nil {
					t.Fatal(err)
				}
			}
			got, err := test.query(t.Context(), owners)
			if err != nil {
				t.Fatalf("query error = %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("owners = %v, want %v", got, test.want)
			}
			if test.foreignExited {
				return
			}
			foreignActive, err := owners.foreign.ListActiveWorkAttempts(t.Context(), WorkAttemptQuery{ProjectID: "detent"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(attemptOwners(foreignActive, "active-"), "foreign") {
				t.Fatalf("live foreign attempt was recovered: %+v", foreignActive)
			}
		})
	}
}

func TestSQLiteWriteTransactionsBeginImmediate(t *testing.T) {
	t.Parallel()
	path := newGenerationDatabase(t)
	holder := openGenerationStore(t, path, 0)
	contender := openGenerationStore(t, path, 50*time.Millisecond)
	tx, err := holder.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = contender.db.ExecContext(t.Context(), "INSERT INTO issue_contract_rollout VALUES ('2026-10-09T00:00:00Z')")
	if !IsBusy(err) {
		t.Fatalf("write during another store's open transaction error = %v, want busy", err)
	}
}

func TestMigrationLockSerializesAcrossOpens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		holder    func(string) string
		contender func(string) string
	}{
		{name: "plain paths", holder: plainPath, contender: plainPath},
		{name: "plain path and uri alias", holder: plainPath, contender: func(path string) string { return "file:" + path + "?mode=rwc" }},
		{name: "uri and plain path", holder: func(path string) string { return "file://localhost" + path }, contender: plainPath},
		{name: "plain path with query", holder: plainPath, contender: func(path string) string { return path + "?_pragma=foreign_keys(1)" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "detent.db")
			holderLock := migrationLockPath(test.holder(path))
			if holderLock != path+".migrate.lock" || migrationLockPath(test.contender(path)) != holderLock {
				t.Fatalf("lock paths holder=%q contender=%q, want %q", holderLock, migrationLockPath(test.contender(path)), path+".migrate.lock")
			}
			release, err := lockMigrations(t.Context(), holderLock)
			if err != nil {
				t.Fatalf("lockMigrations() error = %v", err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
			defer cancel()
			if blocked, err := Open(ctx, Config{Path: test.contender(path)}); !errors.Is(err, context.DeadlineExceeded) {
				if blocked != nil {
					_ = blocked.Close()
				}
				t.Fatalf("Open() while migrations locked error = %v, want deadline exceeded", err)
			}
			if err := release(); err != nil {
				t.Fatalf("release() error = %v", err)
			}
			opened, err := Open(t.Context(), Config{Path: test.contender(path)})
			if err != nil {
				t.Fatalf("Open() after release error = %v", err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func plainPath(path string) string { return path }

func TestMigrationLockPathSkipsOnlyMemoryDatabases(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		path string
		want string
	}{
		{path: ":memory:"},
		{path: ""},
		{path: "file::memory:"},
		{path: "file::memory:?cache=shared"},
		{path: "file:shared?mode=memory&cache=shared"},
		{path: "/data/detent.db", want: "/data/detent.db.migrate.lock"},
		{path: "file:/data/detent.db?mode=rwc", want: "/data/detent.db.migrate.lock"},
		{path: "file:///data/detent.db", want: "/data/detent.db.migrate.lock"},
		{path: "file:/data/my%20detent.db", want: "/data/my detent.db.migrate.lock"},
		{path: "file:detent.db", want: "detent.db.migrate.lock"},
	} {
		if got := migrationLockPath(test.path); got != test.want {
			t.Errorf("migrationLockPath(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestUnknownGenerationLivenessIsSurfacedThenRecovered(t *testing.T) {
	t.Parallel()
	deadPID, deadStart := exitedProcess(t)
	for _, test := range []struct {
		name  string
		pid   int
		start string
	}{
		{name: "process gone", pid: deadPID, start: deadStart},
		{name: "identity mismatch", pid: os.Getpid(), start: deadStart},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := newGenerationDatabase(t)
			now := time.Now().UTC().Truncate(time.Second)
			foreign := openGenerationStore(t, path, 0)
			foreignGeneration := registerGeneration(t, foreign, test.pid, test.start)
			if _, err := foreign.StartWorkAttempt(t.Context(), WorkAttemptStart{
				ProjectID: "detent", IssueID: "foreign", WorkerType: "implement",
				StartedAt: now.Add(-time.Hour), LeaseExpiresAt: now.Add(-time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			current := openGenerationStore(t, path, 0)
			inspectErr := errors.New("process table unavailable")
			current.matchProcess = func(int, string) (bool, error) { return false, inspectErr }
			registerGeneration(t, current, os.Getpid(), selfProcessStart(t))

			foreignLive, err := current.LiveForeignWorkerGenerations(t.Context())
			var unknown *WorkerGenerationLivenessError
			if !errors.As(err, &unknown) || !errors.Is(err, inspectErr) {
				t.Fatalf("LiveForeignWorkerGenerations() error = %v, want unknown liveness", err)
			}
			if !slices.Equal(foreignLive, []int64{foreignGeneration}) || len(unknown.Unknown) != 1 || unknown.Unknown[0].Generation != foreignGeneration || unknown.Unknown[0].PID != test.pid {
				t.Fatalf("live=%v unknown=%+v, want generation %d pid %d treated live", foreignLive, unknown.Unknown, foreignGeneration, test.pid)
			}
			var logs strings.Builder
			unknown.Log(slog.New(slog.NewTextHandler(&logs, nil)))
			for _, want := range []string{"worker_generation=" + strconv.FormatInt(foreignGeneration, 10), "pid=" + strconv.Itoa(test.pid), "process table unavailable"} {
				if !strings.Contains(logs.String(), want) {
					t.Fatalf("liveness log missing %q: %s", want, logs.String())
				}
			}
			skipped, err := current.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: "detent", Now: now})
			if err != nil || len(skipped) != 0 {
				t.Fatalf("timeout with unknown liveness = %+v, %v; want foreign row skipped", skipped, err)
			}

			current.matchProcess = nil
			foreignLive, err = current.LiveForeignWorkerGenerations(t.Context())
			if err != nil || len(foreignLive) != 0 {
				t.Fatalf("after inspection succeeds live=%v err=%v, want dead", foreignLive, err)
			}
			recovered, err := current.TimeoutExpiredWorkAttempts(t.Context(), WorkAttemptTimeout{ProjectID: "detent", Now: now})
			if err != nil || len(recovered) != 1 || recovered[0].IssueID != "foreign" {
				t.Fatalf("timeout after inspection = %+v, %v; want foreign row recovered", recovered, err)
			}
		})
	}
}
