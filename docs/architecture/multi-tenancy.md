# Multi-tenancy architecture

[Back to README](../../README.md#documentation)

This document is normative. It states how Detent serves many organizations and what a change may not do. [INV-23](../invariants.md#inv-23--the-tenant-is-never-a-process) makes its core rule an invariant; [INV-21](../invariants.md#inv-21--designs-hold-at-ten-thousand-tenants-and-runners) sets the scale every design is judged at: 10,000 tenants and 10,000 runners.

## In plain words

A tenant is one customer organization. Detent is one program that serves all of them at once, the way one web server serves every visitor. Each request, background job and database row says which organization it belongs to, and the database refuses to show one organization's rows to another. Detent never starts a separate copy of itself, a separate database connection pool, a separate timer or a separate configuration file for each customer. Those things cost the same whether a customer is busy or asleep, so with 10,000 customers they cost 10,000 times as much. With PostgreSQL's default limit of 100 connections and the smallest pool Detent accepts (3 connections), the database runs out at about 30 customers. When there is more work than one process can carry, Detent runs a few more identical processes and spreads organizations across them; it does not run one per customer.

## The model in one page

- One Hub process serves every organization. A small fleet of identical Hub processes, each assigned a set of organizations through the registry, comes later and only when one process is measured near its limit. This is the pool model in the AWS SaaS Lens and the fully multitenant model in the Azure Architecture Center. Assigning each identical process a subset of organizations is how the Deployment Stamps pattern scales out; a stamp serves many tenants, never one by default.
- The tenant is an attribute of a request, a job and a row. It is never an attribute of a process, a binary, a configuration file, a connection pool, a timer, a goroutine that outlives its work, or a directory.
- The edge authenticates the caller and resolves the organization once. Everything below the edge receives the organization as an explicit typed value and passes it to the tenant store.
- PostgreSQL enforces separation with a tenant key on every tenant-owned row, forced row-level security and a transaction-local organization setting. Process boundaries are not an isolation mechanism and are not relied on.
- Background work is one shared scheduler per process. It pages through tenants with a cursor, does a fixed amount of work per tick, uses a bounded set of workers, sleeps with jitter and gives every tenant a turn.
- Fairness and failure containment are explicit: per-tenant rate and concurrency limits, statement and lock timeouts on every transaction, per-tenant byte and item budgets, and panic recovery at every request and job boundary. Restarting a process is never the way one tenant's failure is contained.
- Dedicated compute for one tenant is the bridge model. It is an explicit, reviewed exception, implemented as a shard of the same pooled Hub whose registry assignment holds one organization; it is never the default and never a separate code path.

## Rules

Each rule is testable by a check, a test or a reviewer reading the diff. "Per-tenant" means the number of instances grows with the number of organizations.

1. No per-tenant process, binary, container or supervisor. Nothing in the hosted serving path starts an operating-system process per organization or keeps one running. Enforced by `TestRepositoryTenancy` (`process_launch`).
2. No per-tenant database pool. A process opens one runtime pool (reader and writer pair) through the pool owner `internal/pgtransport`, sized by that process's concurrency budget. Total server connections equal processes times pool size and do not change when organizations are added. Enforced by `TestRepositoryTenancy` (`pool_open`).
3. No per-tenant timer, ticker or long-lived goroutine. An idle tenant costs no wakeups. Periodic work is a feature of the one shared scheduler in the process, with a fixed per-tick budget, bounded workers, jittered sleep and cursor fairness. A goroutine may exist per tenant only while it serves a request or a step for that tenant, or while a client holds an open stream for that tenant.
4. Tenant identity is resolved once at the edge from the authenticated principal (the verified entry assertion's audience and allocation generation, or the authenticated credential) and is carried explicitly as a typed value (`tracker.OrganizationID` or a scope that contains it) to every store call. There is no ambient, package-level or process-wide "current organization". A path segment alone never selects the tenant. Ratcheted by `TestRepositoryTenancy` (`process_organization`).
5. Every tenant-owned table carries `organization_id`, leading its primary key, with row-level security enabled and forced and a policy on `detent.organization_id`. Every query runs inside a tenant transaction that binds the organization transaction-locally and refuses an empty one. The runtime role has no `BYPASSRLS`. Tenant binding is never a session-level setting on a shared connection.
6. Per-tenant configuration lives in typed database rows or a typed in-memory registry loaded from them, never in a file or directory per tenant on the serving host. Shared configuration (identity provider, billing account, plan catalog defaults) is process configuration and identical for all tenants.
7. Caches are keyed by tenant and bounded in total by entry count or bytes, with eviction. A map that gains an entry per organization and never loses it is a defect, including maps of HTTP transports, limiters, brokers and replay records.
8. One tenant cannot starve or crash the others. Requests have a per-tenant rate limit and a per-tenant share of the serving and background concurrency budgets. Every transaction runs with a statement timeout and a lock timeout. Per-request and per-step work is bounded in rows and bytes. Panics are recovered at each request, scheduler step and background job boundary and become an error attributed to that tenant.
9. No shared lock, connection or transaction is held across a per-tenant network call or a walk over many objects (INV-21). Fan-out across tenants takes the shared resource per item, briefly.
10. Fan-out across tenants is always bounded: a fixed worker count, a semaphore or `errgroup.Group.SetLimit`. Never one unbounded goroutine per tenant, never a loop that visits every tenant in one tick or one request.
11. Lists of tenants are paged. No request, startup path or health check reads or contacts every organization at once.
12. Lifecycle is fenced by registry state and generation. Requests and jobs for an organization that is deleting, deleted or at an older generation are refused at verification time, not by stopping a process.
13. Every change that adds or modifies a hot path, a periodic job or a shared resource states its cost at 10,000 tenants in its Change description (INV-21).
14. Dedicated compute for a tenant is a reviewed exception: a named owner decision recorded on the tenant's registry row as a shard assignment, served by the same binary and code path as every other shard. No per-tenant launcher, configuration file or code branch is added for it.

## What the storage layer already does

| Practice | State today |
|---|---|
| Tenant key on every row, tenant-leading primary key | Done and tested: `internal/hubserver/postgres/schema_test.go:159-172` checks forced row security and a primary key led by `organization_id` for organization-owned tables. |
| Forced row-level security with a tenant policy | Done: for example `internal/hubserver/postgres/migrations/20261031221613_tenant_context.sql:11-19`. PostgreSQL exempts table owners unless security is forced and always exempts `BYPASSRLS` roles. |
| Runtime role cannot bypass policies | Done: roles are `NOBYPASSRLS` (`internal/hubserver/postgres/roles.sql:8-36`); the runtime refuses to start unless `row_security` is on, no organization is bound at session level, and listed tables have forced row security (`internal/hubserver/postgres/runtime.go:35-36`, `runtime.go:111`). |
| Organization bound per transaction, empty refused | Done: `internal/pgtransport/tenant.go:57-59` refuses an empty organization; `tenant.go:82` binds it with `set_config(..., true)`, which lasts only for the transaction. |
| Store calls take the organization explicitly | Done: `tenantstore.Repository.ReadTenant` and `WriteTenant` take `tracker.OrganizationID` (`internal/hubserver/tenantstore/repository.go:65-68`), implemented in `internal/hubserver/postgres/tenant_transaction.go:15-25`. |
| Transaction-pooler-safe client | Done: simple exec mode, no statement or description cache (`internal/pgtransport/pools.go:19-22`). |
| Serving and background concurrency budgets | Per process, not per tenant: `internal/hubserver/postgres/repository.go:43-60`, acquired in `repository.go:169-187`. Per-tenant shares are not implemented. |
| Statement and lock timeouts | Missing as a default. Only one board read sets `statement_timeout` (`internal/hubserver/postgres/board_model_reads.go:286`); nothing sets `lock_timeout`. |
| Shared multi-tenant scheduler | The type exists: `internal/hubmaintenance/scheduler.go:15-20` (32 tenants per tick, 2 workers, 64 items, byte limit), jitter at `scheduler.go:92-98`, per-tenant byte budgets and generation checks in `internal/hubmaintenance/budget.go:23-50`. The Hub feeds it a one-tenant registry (`internal/hubserver/service.go:651-661`). |
| Panic recovery at job boundaries | Missing. Scheduler workers have no `recover` (`internal/hubmaintenance/scheduler.go:127-140`); the only `recover` in Hub production code re-panics (`internal/hubserver/hubrecords/policy.go:84-91`). `net/http` recovers handler panics per request, but not panics in goroutines a handler or job starts. |
| Per-tenant rate limiting | A keyed `golang.org/x/time/rate` limiter exists (`internal/httpratelimit/limiter.go:12-66`) and is used only for OAuth (`internal/cloudentry/oauth.go:30`). It evicts idle keys (`limiter.go:87-98`) but has no hard size bound. |

## Go implementation guidance

Standard library first. `golang.org/x/sync` and `golang.org/x/time` are already direct dependencies (`go.mod:49`, `go.mod:51`); no other package is needed for anything below. The sketches use generic names.

The tenant value flows as an explicit parameter. Context values are for request-scoped data that crosses API boundaries, with an unexported key type; a context is never stored in a struct, and the tenant is never a package variable.

```go
type Tenant struct {
	Organization tracker.OrganizationID
	Generation   int64
}

func (h *Handler) listItems(w http.ResponseWriter, r *http.Request) {
	tenant, err := h.edge.Verify(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	err = h.store.ReadTenant(r.Context(), tenant.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		return h.render(w, reader)
	})
	h.finish(w, r, tenant, err)
}
```

`Verify` checks the assertion signature, takes the organization from its audience, compares the generation with the registry row and rejects a path organization that differs. Below this point no code reads an organization from configuration.

Background work is one loop per process. It pages tenants with a cursor, processes a fixed number per tick with a fixed number of workers, attributes each failure to its tenant and sleeps with jitter. `hubmaintenance.Scheduler` already has this shape; new periodic work is added as a `hubmaintenance.Feature`, not as a new ticker.

```go
const tenantsPerTick, workers = 32, 2

func (s *Sweeper) Tick(ctx context.Context) error {
	page, err := s.registry.Page(ctx, s.cursor, tenantsPerTick)
	if err != nil {
		return fmt.Errorf("page tenants: %w", err)
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
		slots    = make(chan struct{}, workers)
	)
	for _, tenant := range page {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := runStep(ctx, tenant, s.step); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	s.cursor = nextCursor(page, tenantsPerTick)
	return errors.Join(failures...)
}
```

`errgroup.Group` with `SetLimit` is an equivalent bound (its `Go` blocks while the limit is reached) and is already used in `internal/project/manager.go:848-849`. Do not use `errgroup.WithContext` for cross-tenant fan-out: its first error cancels the shared context and one tenant's failure would abort the others.

Recover at every request and job boundary. `recover` regains control only of the panicking goroutine and only from a deferred function; an unrecovered panic crashes the program, and in a pooled Hub that is every tenant.

```go
func runStep(ctx context.Context, tenant Tenant, step func(context.Context, Tenant) error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			slog.ErrorContext(ctx, "tenant step panicked", "organization", tenant.Organization, "stack", string(debug.Stack()))
			err = fmt.Errorf("tenant step: %w", errStepPanicked)
		}
	}()
	return step(ctx, tenant)
}

func recoverRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				if value == http.ErrAbortHandler {
					panic(value)
				}
				slog.ErrorContext(r.Context(), "request panicked", "stack", string(debug.Stack()))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

The error message carries the step, never the panic value, because panic values can contain customer content.

Per-tenant limits live in one bounded map. Entries are created on demand, evicted when idle and capped in total, so 10,000 tenants never mean 10,000 permanent entries plus growth.

```go
type tenantLimits struct {
	mu      sync.Mutex
	max     int
	order   *list.List
	entries map[tracker.OrganizationID]*list.Element
	newRate func() *rate.Limiter
}

type limitEntry struct {
	organization tracker.OrganizationID
	limiter      *rate.Limiter
	inflight     chan struct{}
}

func (l *tenantLimits) acquire(organization tracker.OrganizationID) (*limitEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.entries[organization]; ok {
		l.order.MoveToFront(element)
		entry := element.Value.(*limitEntry)
		return entry, entry.limiter.Allow()
	}
	if l.order.Len() >= l.max {
		oldest := l.order.Back()
		delete(l.entries, oldest.Value.(*limitEntry).organization)
		l.order.Remove(oldest)
	}
	entry := &limitEntry{organization: organization, limiter: l.newRate(), inflight: make(chan struct{}, 4)}
	l.entries[organization] = l.order.PushFront(entry)
	return entry, entry.limiter.Allow()
}
```

`inflight` is the tenant's share of the serving budget: a request takes a slot before it takes the process-wide serving slot (`internal/hubserver/postgres/repository.go:169-187`), so one tenant can hold at most its share of connections.

Pools are sized per process. A tenant is not an input to pool size.

```go
config.MaxConns = int32(serving + background + 1)
pools, err := pgtransport.Open(ctx, readerConfig, writerConfig)
```

Server connections are `hub processes × MaxConns` plus the entry pool. PostgreSQL sizes shared memory from `max_connections`, and pgx's default pool size is the greater of 4 or the CPU count, so a pool per tenant runs out of server connections first. When the fleet grows, put PgBouncer in transaction mode in front; the client is already compatible (`internal/pgtransport/pools.go:19-22`) and the tenant binding is transaction-local. Session-level `SET` does not survive transaction pooling.

Every tenant transaction sets its limits in the same batch that binds the organization (`internal/pgtransport/tenant.go:82`): `set_config('statement_timeout', $2, true)` and `set_config('lock_timeout', $3, true)`. Shutdown cancels the scheduler context, waits for in-flight steps and then closes the one pool.

## Anti-patterns

Each fails at 10,000 tenants for the reason given.

- Process per tenant. 10,000 processes, each with its own pool, timers, caches and memory, all running while idle. With the minimum pool of 3 (`internal/hubserver/postgres/repository.go:47`) and PostgreSQL's default `max_connections` of 100, connections run out at about 30 tenants. The current instance to remove is `ExecLauncher`, which runs `detent hub serve --hosted-config <tenant.yaml> --listen unix:<socket>` per organization (`internal/cloudentry/tenant/launcher.go:226-228`), supervised by one goroutine and a 5 second probe ticker per tenant (`launcher.go:211`, `launcher.go:163`, `launcher.go:367-373`), and wired in `internal/cli/cloud.go:353`.
- Pool per tenant. Connections become tenants times pool size; PostgreSQL memory and process count follow. Today each tenant process opens the same `tenant_postgres` pool configuration (`internal/cli/cloud.go:150`) through `internal/hubserver/postgres/lifecycle.go:42-43`.
- Timer per tenant. Wakeups grow with tenants even when nobody works. Each tenant process runs the maintenance loop about once a second (`internal/hubmaintenance/scheduler.go:92-98`), a periodic maintenance ticker (`internal/hubserver/service.go:722`), billing reconciliation (`internal/hubserver/hosted_billing_worker.go:50`) and the issue mailer (`internal/hubserver/issue_email.go:46`).
- Configuration file per tenant. Files, directories and secrets multiply, drift and cannot be queried or migrated in one transaction. Today: `tenant.yaml` and `admin-token` per organization (`internal/cloudentry/tenant/launcher.go:201`, `launcher.go:486-498`) under a per-organization directory (`internal/cloudentry/provisioning.go:161`).
- Package-level or process-wide tenant state. Any field that holds "the" organization becomes wrong for every other tenant the moment two share the process, and the failure mode is cross-tenant data. Today: `database.hostedOrganization` is set from configuration (`internal/hubserver/service.go:229-230`) and authorization compares against the configured organization (`internal/hubserver/hubauth/request.go:46`, `internal/hubserver/hubauth/operator_checks.go:17`).
- "This process only sees one organization" queries and registries. Code that reads without a tenant filter because the process is single-tenant leaks when it is not. Today the scheduler's registry returns only the configured tenant (`internal/hubserver/service.go:651-661`) and the board feature keeps one cursor in a closure (`service.go:668`).
- Unbounded per-tenant caches. Memory grows with tenants forever. Today the entry caches one HTTP transport with up to 16 idle connections per organization endpoint and never evicts it (`internal/cloudentry/proxy.go:34-57`).
- Fan-out without a limit. 10,000 goroutines, sockets or queries at once. Today the entry starts every ready organization at startup (`internal/cloudentry/provisioning.go:105-113`) from an unpaged list (`internal/cloudentry/registry.go:87-102`).
- Containing a tenant's failure by restarting its process. It hides the defect, multiplies restarts with tenants and does not exist in a pooled Hub. Today the launcher restarts a crashed tenant with backoff (`internal/cloudentry/tenant/launcher.go:215-267`).

## Review checklist

A change that touches hosted serving, tenancy, scheduling, connection pools or background work answers each question in its Change description. A "no" to questions 1 to 11 or a "yes" to question 12 needs an explicit owner decision quoted in the description.

1. Does every new code path get the organization from the verified request or the scheduler's selected tenant, as an explicit parameter?
2. Does it avoid reading an organization from configuration, a package variable or a field set at process start?
3. Does every query run through a tenant transaction for exactly that organization?
4. Does every new tenant-owned table have `organization_id` leading its primary key, forced row security and the tenant policy?
5. Is all new periodic work a feature of the shared scheduler, with no new ticker, timer loop or long-lived goroutine per tenant?
6. Is every fan-out across tenants bounded by a fixed worker count, semaphore or `SetLimit`, and is every tenant list paged?
7. Is every new cache or map keyed by tenant bounded in total, with eviction?
8. Is the change free of new pool construction outside `internal/pgtransport`, and is pool size independent of tenant count?
9. Do requests and steps on this path have a per-tenant limit, run under statement and lock timeouts, and bound their rows and bytes?
10. Is every new goroutine covered by a `recover` that turns a panic into an error attributed to the tenant?
11. Is the change free of shared locks, connections or transactions held across a network call or an object walk?
12. Does the change add a process, binary, file, directory or configuration entry per tenant?
13. What does this cost per request, per tick and in total at 10,000 tenants, in rows, connections, wakeups and memory?
14. Does `TestRepositoryTenancy` pass without raising `internal/invariants/tenancy_policy.json`, and was the baseline lowered for every violation the change removed?

## Today and the target

| Concern | Today | Target |
|---|---|---|
| Serving | One Hub process per organization (`internal/cloudentry/tenant/launcher.go:226-228`), reached by the entry over a per-organization Unix socket (`internal/cloudentry/proxy.go:34-57`, `internal/cloudentry/registry.go:34-37`) | One Hub process serves all organizations; a later fleet of identical processes is assigned organizations through the registry |
| Tenant identity | Configured per process (`internal/hubserver/service.go:229-230`) | Resolved per request from the assertion audience and generation |
| Database | Forced row security and transaction-bound organization, already process-independent (`internal/pgtransport/tenant.go:57-95`) | Unchanged, plus per-transaction statement and lock timeouts |
| Pools | One pool per tenant process (`internal/hubserver/postgres/lifecycle.go:42-43`) | One pool per process, behind PgBouncer when there is more than one process |
| Background work | One scheduler per tenant process over a one-tenant registry (`internal/hubserver/service.go:651-661`) plus per-process tickers | One scheduler per process over all hosted tenants; tickers folded into features |
| Configuration | `tenant.yaml` per organization (`internal/cloudentry/tenant/launcher.go:201`) | Typed rows and the published runtime revisions |
| Failure containment | Process restart (`internal/cloudentry/tenant/launcher.go:215-267`) | Recovery at each request, step and job; tenant-attributed errors |

The migration is ordered work in the Detent tracker; this document states the rules it must reach and does not record its progress. Progress is measured by `internal/invariants/tenancy_policy.json` shrinking to empty.

## Sources

- AWS Well-Architected SaaS Lens, silo, pool and bridge models: https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/silo-pool-and-bridge-models.html
- Azure Architecture Center, tenancy models (single-tenant deployments cost about N times as much; deployments as stamps): https://learn.microsoft.com/azure/architecture/guide/multitenant/considerations/tenancy-models
- Azure Architecture Center, compute for multitenant solutions (shared compute, cache keys prefixed by tenant, quotas): https://learn.microsoft.com/azure/architecture/guide/multitenant/approaches/compute
- Azure Architecture Center, Noisy Neighbor antipattern (resource governance, quotas, query time limits): https://learn.microsoft.com/azure/architecture/antipatterns/noisy-neighbor/noisy-neighbor
- Azure Architecture Center, Deployment Stamps pattern: https://learn.microsoft.com/azure/architecture/patterns/deployment-stamp
- AWS Builders' Library, Fairness in multi-tenant systems (per-tenant quotas and admission control): https://builder.aws.com/content/3Eupj3d2bo4fEvlzYbICMZNhQ3B/fairness-in-multi-tenant-systems (moved from https://aws.amazon.com/builders-library/fairness-in-multi-tenant-systems/)
- AWS Builders' Library, Workload isolation using shuffle-sharding (limiting the blast radius of one tenant once there is a fleet): https://builder.aws.com/content/3F06NpJ8YeoIGP8VHTw4n81pFn8/workload-isolation-using-shuffle-sharding (moved from https://aws.amazon.com/builders-library/workload-isolation-using-shuffle-sharding/)
- PostgreSQL 17, row security policies: https://www.postgresql.org/docs/17/ddl-rowsecurity.html
- PostgreSQL 17, `max_connections`: https://www.postgresql.org/docs/17/runtime-config-connection.html
- PostgreSQL 17, `statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout`: https://www.postgresql.org/docs/17/runtime-config-client.html
- PostgreSQL 17, `set_config` with `is_local`: https://www.postgresql.org/docs/17/functions-admin.html
- PgBouncer pooling modes and features: https://www.pgbouncer.org/features.html
- pgxpool default pool size and concurrency safety: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
- Go `context` package: https://pkg.go.dev/context
- Go blog, Go Concurrency Patterns: Context: https://go.dev/blog/context
- Go blog, Defer, Panic, and Recover: https://go.dev/blog/defer-panic-and-recover
- Go `net/http` Handler panic behavior and `ErrAbortHandler`: https://pkg.go.dev/net/http#Handler
- `golang.org/x/sync/errgroup` `SetLimit`: https://pkg.go.dev/golang.org/x/sync/errgroup
- `golang.org/x/time/rate` Limiter: https://pkg.go.dev/golang.org/x/time/rate
- Google Go Style Guide, global state: https://google.github.io/styleguide/go/best-practices#global-state
