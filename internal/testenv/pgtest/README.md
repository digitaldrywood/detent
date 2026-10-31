# PostgreSQL test fixtures

`pgtest` is a test-only helper for the Hub PostgreSQL port. Existing SQLite
fixtures are unchanged. PostgreSQL schema migrations belong to their owning
package; supply them as the template migration function.

Keep one `pgtest.NewTemplate(migrate)` in a package variable per test process.
Call `template.Database(t)` to get an isolated `*sql.DB`, and call
`template.Close(ctx)` after `m.Run()` in `TestMain`, before `os.Exit`. Give the
close context the test process deadline. Fixture cleanup is registered with
`t.Cleanup`; close application connections before fixture cleanup runs. Run
with a nonzero `go test -timeout` so setup and cleanup have a deadline.

The migration function receives an empty database and must finish all migration
work before returning. It runs once, then its connection pool is closed and the
template disables connections. Each fixture clones that database with
`CREATE DATABASE ... TEMPLATE`, using a random name and its own connection pool.
Register application database cleanup after obtaining the fixture so it runs
before the fixture database is dropped.

Select the server explicitly with `DETENT_TEST_POSTGRES`:

- `binary`: run `initdb` and `postgres` from PATH. Use PostgreSQL 17 binaries.
  The cluster lives under `TMPDIR`, listens only on a unique Unix socket, and is
  stopped and removed by `template.Close`. This mode requires Unix socket support.
- `postgres://example:fake@127.0.0.1:55432/postgres`: attach to a disposable local
  PostgreSQL 17, including a container published on a loopback port. Supply an
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

For example, from a linked worktree with PostgreSQL 17 on PATH:

```sh
mkdir -p "$PWD/tmp"
TMPDIR="$PWD/tmp" DETENT_TEST_POSTGRES=binary \
  go test -race ./internal/testenv/pgtest -timeout=120s
```

At most four fixture databases are live per template owner, with two client
connections per fixture and one administrative connection. This bounds the
normal process total at nine connections, regardless of the number of test
cases or tenants. The admin pool serializes cloning. Ownership checks use exact
generated names; cleanup never enumerates or drops other processes' databases.
Failed migration and fixture setup close their connection pools and drop their
databases, preserving the primary error. Cleanup detaches cancellation while
retaining the test deadline. Process readiness and parallel test coordination
use signals, without polling, sleeps, or fixture-specific timeouts.
