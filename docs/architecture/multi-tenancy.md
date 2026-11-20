# Multi-tenancy architecture

[Back to README](../../README.md#documentation)

This architecture contract is normative. [AGENTS.md](../../AGENTS.md#authority) incorporates it through [INV-23](../invariants.md#inv-23--the-tenant-is-never-a-process); where the two differ, AGENTS.md and the invariant win. Other pages under `docs/` remain reference material. Every design is judged at the scale in [INV-21](../invariants.md#inv-21--designs-hold-at-ten-thousand-tenants-and-runners): 10,000 tenants and 10,000 runners.

These rules are Detent's product architecture, decided by the owner. The sources at the end describe the models and trade-offs (pool, silo, bridge, stamps, noisy neighbors); they do not prescribe these rules, and they say that sharing resources always carries some noisy-neighbor risk. The rules below say how Detent bounds that risk.

## In plain words

A tenant is one customer organization. Detent is one program that serves all of them at once, the way one web server serves every visitor. Each request, background job and database row says which organization it belongs to, and the database refuses to show one organization's rows to a transaction bound to another. Detent does not start a separate copy of itself, a separate database pool, a separate timer or a separate configuration file for each customer, because those cost the same whether a customer is busy or asleep and multiply with every customer added. When one process is not enough, Detent runs a few more identical processes and assigns organizations to them. A shared process is not a hard wall between customers: a crash, a memory exhaustion or a leaked secret in that process affects everyone it serves, so the rules also bound how much one customer can use and how a failure is contained.

## The model

- One Hub process serves many organizations. Later, a small fleet of identical Hub processes (shards) each serves the organizations the registry assigns to it, the way a deployment stamp serves a set of tenants.
- The tenant is an attribute of a request, a job and a row. It is never an attribute of a process, binary, configuration file, connection pool, timer or directory.
- A verified scope is built once, at the request edge or by the shared scheduler, and passed explicitly. Current state, generation and membership are checked before reads and again where writes commit.
- PostgreSQL row security separates tenants' rows. It cannot tell whether application code bound the right organization; the verified scope does that.
- Background work runs through one shared scheduler per process with a fixed per-tick budget.
- Fairness and failure handling are bounded and tested, not assumed.
- Dedicated compute for one organization is a shard assignment approved by the owner, never a per-tenant launcher.

## Rules

"Per-tenant" means the number of instances grows with the number of organizations. A rule names its check when one exists; everything else is enforced by review.

1. No per-tenant process. Nothing starts, supervises or keeps an operating-system process, binary or container per organization. The only dedicated placement is the rule 20 exception, which uses the same shard machinery.
2. Physical pools do not grow with tenants. Count distinct physical pools: one runtime reader and writer pair per Hub process (they may be one pool), the entry pool, and a migration pool that runs outside serving. A wrapper around an existing pool is not a new pool. `MaxConns` is a ceiling, not a count of open connections; the server-side ceiling is the sum of `MaxConns` over physical pools. The process host owns pool lifecycle: adding or removing an organization never constructs or closes a physical pool, wherever the factory lives.
3. No per-tenant timer or idle goroutine. An idle tenant costs no wakeups. A goroutine works for one tenant only while it serves a request, runs a scheduler step, runs an admitted asynchronous job that has a deadline, is cancelled with its owner and is joined before shutdown, or serves an admitted stream with a memory bound.
4. One verified scope. Scopes are built only in one narrow authority package: its request constructor works from the authenticated principal (the verified entry assertion's audience and allocation generation, or a verified credential), and its scheduler constructor works from the registry page the shared scheduler selected. A path organization must equal the audience. The scope is immutable and passed explicitly, and every tenant store entry point rejects a zero or invalid scope; nothing reads a process-wide "current organization", and a bare organization ID string is not proof of authority. Current registry state, generation and membership are checked when the scope is built, before any read, and again inside the transaction where a write commits. An external publication is authorized and reserved by a transaction; the external call runs after it, outside any transaction, under the existing publication owner (rule 15). During migration, one process may hold several tenant-bound services, each built for one organization and never re-pointed per request.
5. Database isolation. Tenant tables lead their primary key with `organization_id`, use foreign keys that include `organization_id`, enable and force row security, and use a policy with both `USING` and `WITH CHECK` on the transaction-local organization. The runtime login is `NOSUPERUSER` and `NOBYPASSRLS`, owns no tables, and is not a member of the migration or platform roles. A missing binding fails closed. The only paths outside tenant policies are named: the `platform` schema used by the entry, migration administration, and `SECURITY DEFINER` functions with a fixed `search_path` and narrow grants (search reader, operator metadata page, designated local organization lookup). A new exception needs review. Effective policies, grants, views, partitions and function owners are audited against migrated PostgreSQL, not inferred from migration text.
6. Pooled connection reuse. A pooled runtime connection carries no session-level tenant state: no `SET` or `set_config(..., false)` of tenant settings, no session advisory locks, temporary tables, `LISTEN` or per-tenant prepared statements. The organization is bound with `set_config(..., true)` inside the transaction. Tests prove a connection reused after commit, rollback and cancellation sees no earlier binding.
7. Configuration. Process configuration holds infrastructure defaults shared by every organization. Organization-specific settings (plan, billing binding, identity-provider organization, workspace policy, overrides) live in typed rows keyed by organization and revised by revision or generation. No file or directory owns an organization's configuration or lifecycle. An item may retain its worktree across phases under [INV-17](../invariants.md#inv-17--hub-owned-assignment-and-retained-item-worktrees). Execution authority stays in the Hub.
8. Secrets and outbound calls. Tenant secrets, credential lookups and outbound destinations are resolved through the verified scope. Encryption binds the organization (and project where applicable) as context and supports key rotation. Tenant secrets never enter the process environment and never sit in a cache that is not keyed by the scope. A shared process is not a hard secrets boundary; the design keeps each secret's lifetime to the operation that uses it.
9. Caches and admission state. A cache is keyed by organization, plus generation or revision and principal or project wherever the result depends on them, and is bounded in total with eviction. Admission, rate, replay and fencing state is not content: an active entry is never evicted, waiters are bounded, a full table applies backpressure, and eviction never refills an allowance.
10. Admission and fairness. Requests and jobs pass per-tenant rate and concurrency limits owned by one policy owner. Requests, jobs and pool acquisition have total deadlines; transactions have a finite lifetime, and every tenant transaction sets transaction-local statement and lock timeouts supplied by an existing policy owner (no new configuration keys created to work around a mechanism, per INV-3). Per-request and per-step work is bounded in rows and bytes. Fairness is shown by a test in which one tenant saturates its limits while another still completes.
11. Failure boundaries. Each request, scheduler step, background job and started goroutine has its own `recover`, and every started goroutine is joined. A recovered panic becomes an error attributed to the tenant and is logged once, at the handling edge, with sanitized diagnostics: function and line frames, no argument values, no customer content, no secrets. If the response was already committed, the connection is aborted instead of writing a 500. A panic after an uncertain external write is reconciled by the existing retry owner, never retried blindly. `recover` does not contain out-of-memory, fatal runtime errors such as concurrent map writes, or memory corruption; the process is not a hard crash boundary, and sharding limits the blast radius.
12. Shared resources. No shared lock, connection or transaction is held across a network call or a walk over many objects, and no long object or network work hides inside a shared transaction ([INV-21](../invariants.md#inv-21--designs-hold-at-ten-thousand-tenants-and-runners)).
13. Bounded work. Cross-tenant work selects a bounded page (and rejects a page longer than requested), keeps no unbounded queue, gives each step a deadline and runs at most a fixed number of steps at once. Slot acquisition honors cancellation. A semaphore alone bounds concurrency, not total work.
14. Startup and readiness. Process startup and readiness are bounded independently of the number of organizations and of any one unavailable organization. Nothing enumerates or contacts every organization before serving, and health reports bounded aggregates.
15. Lifecycle fencing. Organization state and generation are checked inside the transaction where a mutation commits. External publication has no shared transaction with PostgreSQL: a transaction authorizes and reserves it, the call runs outside the transaction, and the existing publication owner handles generation changes and uncertain outcomes through its receipt and reconciliation protocol. A database check cannot atomically fence an external system that has no fencing facility. Deleting or reassigning an organization cancels and drains its jobs and streams, rejects late results, and refuses cache entries or runtime revisions from an older generation. A check only at request verification races deletion; cancellation alone cannot fence a late write.
16. Export and deletion. Export and deletion authorize the scope, page their work, fence writers and jobs, cover object storage and caches, and honor retention and backup obligations.
17. Schema migration. Migration credentials stay outside serving processes. Per-tenant backfills are bounded, resumable and run under their existing owners. Each change states how old and new processes behave against the same schema during rollout.
18. Observability. Logs and traces carry the organization ID as a protected attribute and never carry customer content or secrets. Metric labels have bounded cardinality: no issue, user or request IDs as labels.
19. Cost at scale. Every change that adds or modifies a hot path, periodic job or shared resource states its cost at 10,000 tenants, with the evidence INV-21 already requires.
20. Dedicated compute is an approved exception. It requires Cory's explicit approval and a recorded reason on the organization's registry assignment, and it uses the ordinary shard machinery: the same binary, scope constructor, scheduler, pools and configuration path. It never restores a per-tenant launcher, configuration file or code branch.

## Go guidance

Standard library first. `golang.org/x/sync` and `golang.org/x/time` are already direct dependencies; nothing below needs another package. Names are generic.

The scope type lives in a narrow authority package that holds only its constructors and accessors, and it is passed as a value. Context values are only for request-scoped data with an unexported key type; a context is never stored in a struct, and the scope is never a package variable.

```go
type Scope struct {
	organization tracker.OrganizationID
	generation   int64
	principal    string
}

func (e *Edge) Verify(ctx context.Context, r *http.Request) (Scope, error) {
	claims, err := e.assertions.Verify(r)
	if err != nil {
		return Scope{}, err
	}
	if r.PathValue("organization") != claims.Audience {
		return Scope{}, errScopeMismatch
	}
	current, err := e.registry.Organization(ctx, claims.Audience)
	if err != nil || current.State != "ready" || current.Generation != claims.Generation {
		return Scope{}, errScopeStale
	}
	if err := e.members.Require(ctx, current.ID, claims.Subject); err != nil {
		return Scope{}, err
	}
	return Scope{organization: current.ID, generation: current.Generation, principal: claims.Subject}, nil
}

func (s Scope) Valid() bool {
	return s.organization != "" && s.generation > 0
}
```

Unexported fields stop code outside the authority package from filling in a scope; code inside that package can still build one, and any caller can declare a zero value. So the package stays small, and every tenant store entry point rejects a zero or invalid scope before it opens a transaction. Registry state, generation and membership are checked in `Verify` before any read, because a recheck at write time does not protect a stale read. The scheduler's constructor in the same package builds job scopes from the registry page it selected; today `hubmaintenance` binds the selected tenant and generation into each step through `Budget.Context` and checks them with `RequireTenant`.

Cross-tenant background work is one loop per process. `hubmaintenance.Scheduler` (`internal/hubmaintenance`) already has the shape: a cursor over a registry page, a per-tick tenant limit, a fixed worker count, per-tenant byte and item budgets, generation checks and jittered sleep. New periodic work becomes a feature of that scheduler, not a new ticker. A tick looks like this:

```go
func (s *Sweeper) Tick(ctx context.Context) error {
	page, err := s.registry.Page(ctx, s.cursor, tenantsPerTick)
	if err != nil {
		return fmt.Errorf("page tenants: %w", err)
	}
	if len(page) > tenantsPerTick {
		return errPageTooLong
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
		slots    = make(chan struct{}, workers)
	)
	dispatched := 0
dispatch:
	for _, tenant := range page {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		dispatched++
		wg.Go(func() {
			defer func() { <-slots }()
			stepCtx, cancel := context.WithTimeout(ctx, s.stepDeadline)
			defer cancel()
			if err := runStep(stepCtx, tenant, s.step); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if dispatched > 0 {
		s.cursor = page[dispatched-1].Organization
	}
	if dispatched == len(page) && len(page) < tenantsPerTick {
		s.cursor = ""
		s.nextFeature()
	}
	return errors.Join(append(failures, ctx.Err())...)
}
```

The cursor advances only through tenants that were dispatched, and it wraps to the start (and the next feature) only after a short page was dispatched completely, so a cancelled tick never skips a tenant's turn. `hubmaintenance.Scheduler.Tick` follows the same rule. `errgroup.Group.SetLimit` bounds concurrency too, but a `Go` call blocked on the limit does not observe cancellation; take a cancellation-aware slot first, as above, or use `TryGo`. Do not use `errgroup.WithContext` across tenants: its first error cancels the others.

Each step and each started goroutine recovers its own panic and returns an error; it does not log. The caller that handles the error logs once.

```go
type PanicError struct {
	Organization tracker.OrganizationID
	Frames       []string
}

func (e *PanicError) Error() string { return "tenant step panicked" }

func runStep(ctx context.Context, tenant Tenant, step func(context.Context, Tenant) error) (err error) {
	defer func() {
		if recover() != nil {
			err = &PanicError{Organization: tenant.Organization, Frames: callerFrames(3)}
		}
	}()
	return step(ctx, tenant)
}

func callerFrames(skip int) []string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(skip, pcs)])
	var out []string
	for frame, more := frames.Next(); ; frame, more = frames.Next() {
		out = append(out, fmt.Sprintf("%s:%d", frame.Function, frame.Line))
		if !more {
			return out
		}
	}
}
```

`callerFrames` records function names and lines without argument values, unlike `debug.Stack`. The panic value is dropped because it can contain customer content. The HTTP boundary is the handling edge, so it logs there, and it aborts the connection when the response is already committed:

```go
func recoverRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &committedWriter{ResponseWriter: w}
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			if value != http.ErrAbortHandler {
				slog.ErrorContext(r.Context(), "request panicked", "frames", callerFrames(3))
			}
			if value == http.ErrAbortHandler || tracked.committed {
				panic(http.ErrAbortHandler)
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(tracked, r)
	})
}
```

`committedWriter` sets `committed` on the first `WriteHeader` or `Write`.

Per-tenant rate limits use `golang.org/x/time/rate` limiters held in one table that follows rule 9: active entries pinned, waiters bounded, backpressure when full, and no fresh allowance from eviction. A per-tenant concurrency share is taken before the process-wide serving slot, so one organization can hold at most its share of connections. Write this owner once, with tests, and reuse it; do not copy a sketch.

Pools are sized per process. The owner creates them once and the host closes them once:

```go
config.MaxConns = int32(serving + background + 1)
pools, err := pgtransport.Open(ctx, readerConfig, writerConfig)
```

PostgreSQL sizes shared memory from `max_connections`, and pgx defaults `MaxConns` to the greater of 4 and the CPU count. The Detent client uses `QueryExecModeExec` (extended protocol without a statement cache) and transaction-local tenant binding, so it can run behind a transaction pooler such as PgBouncer. Whether to deploy one is a measured choice, not a requirement of having a second process. Under transaction pooling, session-level state is forbidden by rule 6 because it would leak to the next borrower.

Each tenant transaction sets its timeouts in the same batch that binds the organization (`internal/pgtransport` `BeginBatch`): `set_config('statement_timeout', $2, true)` and `set_config('lock_timeout', $3, true)`, with values from the existing policy owner. Request and job contexts carry the total deadline that covers pool acquisition and transaction lifetime. Shutdown cancels the scheduler context, waits for in-flight steps and jobs, then closes each pool once.

## Required tests

- Pooled connection reuse: a connection reused after commit, rollback, cancelled rollback and a cancelled query sees only its new binding. `TestTransactionCleanup` and `TestTenantBinding` in `internal/pgtransport` assert this today and must stay.
- Row security catalog audit: under the real runtime login against migrated PostgreSQL, every application table either has forced row security with a tenant policy or appears in rule 5's named exceptions; the runtime role's attributes, memberships and ownership match rule 5.
- Two-organization host lifecycle (required for the pooled Hub, not writable against today's code): one host serves two organizations; adding and removing an organization constructs and closes no physical pool and starts no scheduler or timer; each organization's requests and steps read only its rows; a panic in one organization's step leaves the other's next step running; deleting one organization fences its late writes.
- Fairness: one organization saturating its rate and concurrency limits does not stop another organization's request and step from completing.

## Anti-patterns

Each fails at 10,000 tenants. The named code is a stable example; the migration work is in the Detent tracker.

- Process per tenant. Processes, pools, timers, caches and memory multiply and all run while idle. Example: `ExecLauncher` in `internal/cloudentry/tenant` runs `detent hub serve --hosted-config <tenant.yaml>` per organization and restarts it on failure.
- Pool per tenant. Server connection ceilings multiply with organizations. Example: every per-organization Hub process opens its own pool through `internal/hubserver/postgres` `Open`.
- Timer per tenant. Wakeups grow with organizations even when none of them is working. Example: each per-organization Hub process runs its own maintenance loop and periodic tickers.
- Configuration file per tenant. Files drift, cannot be changed in one transaction and cannot be queried. Example: `tenant.yaml` and `admin-token` per organization directory.
- Process-wide current organization. A field holding "the" organization is wrong for every other tenant in the same process, and the failure is cross-tenant data. Example: `hostedOrganization` set from `Config.Hosted.OrganizationID`.
- Single-organization registries and cursors. Code that assumes it sees one organization leaks or starves when it sees many. Example: the maintenance registry that returns only the configured organization.
- Unbounded per-tenant maps. Memory grows with every organization ever seen. Example: an HTTP transport cached per organization endpoint and never evicted.
- Unbounded fan-out and enumeration. Thousands of goroutines, sockets or queries at once, and startup that waits on every organization.
- Restarting a process to contain one tenant's failure. It hides the defect and does not exist in a pooled Hub.

## Review checklist

A change that touches hosted serving, tenancy, scheduling, connection pools or background work answers each question in its Change description. "Not applicable, because ..." is an acceptable answer when the reason names why the change cannot affect that question; it never waives an applicable rule. A "no" to an applicable question, or a "yes" to question 12, needs an owner decision quoted in the description.

1. Does every new path get its scope from the authority package's request or scheduler constructor, passed explicitly, and do store entry points reject a zero scope?
2. Are state, generation and membership checked before reads and inside the committing transaction, and does external publication go through its owner's reservation, receipt and reconciliation?
3. Does every query run in a tenant transaction for exactly that organization, with no session-level tenant state?
4. Does every new tenant table lead its primary key and foreign keys with `organization_id`, with forced row security and `USING` plus `WITH CHECK`?
5. Is new periodic work a scheduler feature, and does every new goroutine have a deadline, cancellation, its own `recover` and a join?
6. Are pages, queues and step time bounded, and does slot acquisition honor cancellation?
7. Are new caches keyed by organization and by every input the result depends on, and bounded in total? Is admission state never evicted while active?
8. Is every physical pool created by the process host once, independent of organization count?
9. Do requests, jobs and transactions have total deadlines and transaction-local statement and lock timeouts, and per-tenant rate and concurrency limits?
10. Are panics turned into tenant-attributed errors logged once with sanitized frames, and are uncertain external writes reconciled rather than retried?
11. Are secrets, credentials and outbound destinations resolved through the scope, and are logs, traces and metrics free of customer content and unbounded labels?
12. Does the change add a process, binary, file, directory, timer or configuration entry per organization?
13. Do startup, readiness, export, deletion and migration stay bounded and fenced as rules 14 to 17 require?
14. What does the change cost per request, per tick and in total at 10,000 tenants?

## Sources

- AWS Well-Architected SaaS Lens, silo, pool and bridge models: https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/silo-pool-and-bridge-models.html
- Azure Architecture Center, tenancy models: https://learn.microsoft.com/azure/architecture/guide/multitenant/considerations/tenancy-models
- Azure Architecture Center, compute for multitenant solutions (cache keys by tenant, quotas, noisy-neighbor risk): https://learn.microsoft.com/azure/architecture/guide/multitenant/approaches/compute
- Azure Architecture Center, Noisy Neighbor antipattern (the risk cannot be fully avoided; governance, quotas, query limits): https://learn.microsoft.com/azure/architecture/antipatterns/noisy-neighbor/noisy-neighbor
- Azure Architecture Center, Deployment Stamps pattern: https://learn.microsoft.com/azure/architecture/patterns/deployment-stamp
- AWS Builders' Library, Fairness in multi-tenant systems: https://builder.aws.com/content/3Eupj3d2bo4fEvlzYbICMZNhQ3B/fairness-in-multi-tenant-systems
- AWS Builders' Library, Workload isolation using shuffle-sharding: https://builder.aws.com/content/3F06NpJ8YeoIGP8VHTw4n81pFn8/workload-isolation-using-shuffle-sharding
- PostgreSQL 17, row security policies (owners, FORCE, BYPASSRLS, default deny): https://www.postgresql.org/docs/17/ddl-rowsecurity.html
- PostgreSQL 17, `max_connections`: https://www.postgresql.org/docs/17/runtime-config-connection.html
- PostgreSQL 17, statement and lock timeouts: https://www.postgresql.org/docs/17/runtime-config-client.html
- PostgreSQL 17, `set_config` with `is_local`: https://www.postgresql.org/docs/17/functions-admin.html
- PgBouncer pooling modes and session features: https://www.pgbouncer.org/features.html
- pgxpool configuration: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
- pgx query execution modes: https://pkg.go.dev/github.com/jackc/pgx/v5#QueryExecMode
- Go `context`: https://pkg.go.dev/context and https://go.dev/blog/context
- Go, Defer, Panic, and Recover: https://go.dev/blog/defer-panic-and-recover
- Go `net/http` Handler panics and `ErrAbortHandler`: https://pkg.go.dev/net/http#Handler
- Go `runtime.CallersFrames`: https://pkg.go.dev/runtime#CallersFrames
- `golang.org/x/sync/errgroup`: https://pkg.go.dev/golang.org/x/sync/errgroup
- `golang.org/x/time/rate`: https://pkg.go.dev/golang.org/x/time/rate
- Google Go Style Guide, global state: https://google.github.io/styleguide/go/best-practices#global-state
