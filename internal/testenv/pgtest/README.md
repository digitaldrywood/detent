# PostgreSQL test fixtures

`pgtest` is a test-only helper for the Hub PostgreSQL port. Existing SQLite
fixtures are unchanged. PostgreSQL schema migrations belong to their owning
package; supply them as the template migration function.

Keep a `pgtest.NewTemplate(migrate)` in a package variable for the test process.
The first constructed Template owns the binary cluster's lifetime, including
when another Template initializes first. Close that owner after `m.Run()` in
`TestMain`, before `os.Exit`, even if the owner never needed a database. Give the
close context the test process deadline. Other Templates borrow the same server;
their `Close` drops only their own databases. Close all borrowers before the
lifetime owner. A closed lifetime owner cannot start another cluster.

Call `template.Database(t)` to get an isolated `*sql.DB`. Fixture cleanup is
registered with `t.Cleanup`; close application connections before fixture cleanup
runs. Run with a nonzero `go test -timeout` so setup and cleanup have a deadline.

The migration function receives an empty database and must finish all migration
work before returning. It runs once, then its connection pool is closed and the
template disables connections. Each fixture clones that database with
`CREATE DATABASE ... TEMPLATE`, using a random name and its own connection pool.
Register application database cleanup after obtaining the fixture so it runs
before the fixture database is dropped.

Select the server explicitly with `DETENT_TEST_POSTGRES`:

- `binary`: run `initdb` and `postgres` from PATH. Use PostgreSQL 17 or 18 binaries.
  The cluster lives under `TMPDIR`, listens only on a unique Unix socket, and is
  stopped and removed by the lifetime owner's `Close`. Every Template in the
  test binary shares this cluster. A shell supervisor owns the postmaster
  and reads a lifetime pipe whose only writer is the test binary. Normal close
  requests fast shutdown; EOF after an abrupt exit requests immediate shutdown.
  The supervisor waits for the postmaster to release its IPC resources. This
  mode requires Unix sockets and `/bin/sh`. It uses `fsync=off`,
  `synchronous_commit=off` and `full_page_writes=off`, following PostgreSQL's
  [non-durable settings](https://www.postgresql.org/docs/current/non-durability.html)
  for disposable correctness tests. Crash-recovery and durability tests must use
  a normally durable disposable URL server instead. URL attachment never changes
  server durability settings.
- `postgres://example:fake@127.0.0.1:55432/postgres`: attach to a disposable local
  PostgreSQL 17 or 18, including a container published on a loopback port. Supply an
  explicit port and a role permitted to create databases. IPv6 loopback and
  `localhost` are supported. For a Unix socket, add `?host=/absolute/socket/path`.
  The helper leaves the attached server running.
- Unset or empty: PostgreSQL tests skip with configuration instructions. They
  never use SQLite as a fallback. Invalid settings and incorrect server versions
  fail instead of skipping.

Unset ambient `PG*` variables before using a configured server. The helper
rejects them and constructs the driver configuration directly, without probing
standard socket directories or user account files. Connection settings come only
from the explicit test selection. Attachment URLs accept only loopback addresses
and the optional Unix socket parameter. Error messages do not print URLs or
passwords. Never point this helper at a production server.

For example, from a linked worktree with PostgreSQL 17 or 18 on PATH:

```sh
mkdir -p "$PWD/tmp"
TMPDIR="$PWD/tmp" DETENT_TEST_POSTGRES=binary \
  go test -race ./internal/testenv/pgtest -timeout=120s
```

In binary mode, at most four top-level tests own fixtures across all Templates
at once, with one shared administrative connection. URL attachment retains
per-Template admission and administrative pools. A test and its subtests share
admission, so requesting another database cannot wait on a slot held by that same
test. Every database remains isolated, with two client connections per database.
Total connections depend on the number of databases those tests keep open; close
unused pools promptly. The admin pool serializes cloning. Each Template retains
its own exact-name ownership set; closing a borrower never drops another
Template's databases. Cleanup never enumerates other processes' databases.

Failed migration and fixture setup close their connection pools and drop their
databases, preserving the primary error. Even a failed lifetime-owner migration
keeps the shared server available until the owner explicitly closes it. Cleanup
detaches cancellation while
retaining the test deadline. Process readiness and parallel test coordination
use signals, without polling, sleeps, or fixture-specific timeouts.

The lifetime pipe uses the operating system's [EOF semantics](https://man7.org/linux/man-pages/man7/pipe.7.html)
and Go's [file-backed subprocess input](https://pkg.go.dev/os/exec#Cmd) so a killed
test binary closes the writer without running Go cleanup. The supervisor uses
an explicitly saved descriptor for the watcher's input because asynchronous
shell commands can [replace standard input with `/dev/null`](https://www.gnu.org/software/bash/manual/html_node/Lists.html).
The extra descriptor is closed in the postmaster and after watcher setup.
The supervisor uses
PostgreSQL's [documented shutdown signals](https://www.postgresql.org/docs/17/server-shutdown.html),
never SIGKILL. Its abrupt-exit path sends SIGTERM before SIGQUIT so termination
also works before PostgreSQL replaces the signal dispositions inherited from
the shell. PostgreSQL's default memory settings are retained: even anonymous
`mmap` requires a small [System V segment](https://www.postgresql.org/docs/17/kernel-resources.html#SYSVIPC),
so correct shutdown remains necessary.

`template.OpenRole(t, fixture, role, password)` opens a separately authenticated
single-connection pool to an owned fixture database, using only the template's
explicit server configuration. Provision the disposable test login and its
grants through the fixture first. The helper registers pool cleanup before the
fixture is dropped and never prints connection credentials. It disables idle
fixture administrator connections so two restricted pools can share the existing
two-connection database limit. Close role pools before dropping their test
logins. With role pools, budget one active setup connection and two restricted
connections per fixture in addition to the template's administrative connection.
