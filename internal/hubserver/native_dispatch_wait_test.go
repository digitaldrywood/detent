package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dispatchQueryProbe struct {
	queries          atomic.Int64
	candidates       atomic.Int64
	rows             atomic.Int64
	query            atomic.Pointer[dispatchProbedQuery]
	completionBodies atomic.Int64
	maxArgumentBytes atomic.Int64
}

type dispatchProbedQuery struct {
	statement string
	args      []any
}

type dispatchProbeDriver struct {
	driver.Driver
	probe *dispatchQueryProbe
}
type dispatchProbeConn struct {
	driver.Conn
	probe *dispatchQueryProbe
}
type dispatchProbeRows struct {
	driver.Rows
	probe *dispatchQueryProbe
}

func (d dispatchProbeDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return dispatchProbeConn{conn, d.probe}, nil
}

func (c dispatchProbeConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}

func (c dispatchProbeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c dispatchProbeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.probe.queries.Add(1)
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil && strings.HasPrefix(query, "WITH candidates AS") {
		c.probe.candidates.Add(1)
		values := make([]any, 0, len(args))
		argumentBytes := int64(0)
		for _, arg := range args {
			values = append(values, arg.Value)
			if value, ok := arg.Value.(string); ok {
				argumentBytes += int64(len(value))
			}
		}
		for old := c.probe.maxArgumentBytes.Load(); argumentBytes > old; old = c.probe.maxArgumentBytes.Load() {
			if c.probe.maxArgumentBytes.CompareAndSwap(old, argumentBytes) {
				break
			}
		}
		c.probe.query.Store(&dispatchProbedQuery{query, values})
		return dispatchProbeRows{rows, c.probe}, nil
	}
	return rows, err
}

func (r dispatchProbeRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil {
		r.probe.rows.Add(1)
		for index, column := range r.Columns() {
			if strings.Contains(column, "completion_evidence") && values[index] != nil && values[index] != "" {
				r.probe.completionBodies.Add(1)
			}
		}
	}
	return err
}

func measureDispatchQueries(t *testing.T, s *Service) *dispatchQueryProbe {
	t.Helper()
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	if s.outbox != nil {
		s.outbox.stop()
	}
	if s.workspaces != nil {
		s.workspaces.Stop()
	}
	if err := s.database.db.Close(); err != nil {
		t.Fatal(err)
	}
	probe := &dispatchQueryProbe{}
	name := newNativeID("dispatch_probe")
	sql.Register(name, dispatchProbeDriver{s.database.db.Driver(), probe})
	db, err := sql.Open(name, sqliteDSN(s.database.path, s.config.BusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s.database.db = db
	return probe
}

func serveDispatchWait(ctx context.Context, f nativeFixture, token, cursor string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, f.base+"/claims/wait?wait=30&after="+url.QueryEscape(cursor), nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.service.echo.ServeHTTP(response, request)
	return response
}

func dispatchWaitCursor(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	requireNativeStatus(t, response, http.StatusOK)
	var page struct {
		Cursor string `json:"cursor"`
	}
	decodeHubResponse(t, response, &page)
	if page.Cursor == "" {
		t.Fatal("empty wait cursor")
	}
	return page.Cursor
}

func awaitDispatchSubscribers(t *testing.T, s *Service, organization tracker.OrganizationID, count int) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		s.notifications.mu.Lock()
		got := len(s.notifications.subscribers[dispatchNotificationKey(organization)])
		s.notifications.mu.Unlock()
		if got == count && s.database.db.Stats().InUse == 0 {
			return nil
		}
		return fmt.Errorf("subscribers=%d, want %d", got, count)
	})
}

type dispatchWaitPostgresTrace struct {
	queries    atomic.Int64
	candidates atomic.Int64
	pool       atomic.Pointer[pgxpool.Pool]
}

func (p *dispatchWaitPostgresTrace) record(statement string) {
	p.queries.Add(1)
	if strings.HasPrefix(strings.TrimSpace(statement), "SELECT i.id,i.revision,i.number,i.native_id,") {
		p.candidates.Add(1)
	}
}
func (p *dispatchWaitPostgresTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.record(data.SQL)
	return ctx
}
func (*dispatchWaitPostgresTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (p *dispatchWaitPostgresTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, q := range data.Batch.QueuedQueries {
		p.record(q.SQL)
	}
	return ctx
}
func (*dispatchWaitPostgresTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}
func (*dispatchWaitPostgresTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
func (p *dispatchWaitPostgresTrace) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	p.pool.Store(pool)
	return ctx
}
func (*dispatchWaitPostgresTrace) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
}

type dispatchWaitMutationRepository struct {
	*postgres.Repository
	beforeWrite      func()
	reject           atomic.Bool
	credentialOutage atomic.Bool
	credentialID     string
	aliasCapacity    bool
}

func (r *dispatchWaitMutationRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		if err := write(w); err != nil {
			return err
		}
		if r.reject.Load() {
			return errors.New("fixture commit refused")
		}
		return nil
	})
}

func (r *dispatchWaitMutationRepository) WriteRunnerHeartbeat(ctx context.Context, organization tracker.OrganizationID, credentialID, runnerID string, write func(context.Context, tenantstore.Writer) error) error {
	return r.Repository.WriteRunnerHeartbeat(ctx, organization, credentialID, runnerID, func(ctx context.Context, w tenantstore.Writer) error {
		if r.aliasCapacity {
			w = dispatchWaitCapacityWriter{w}
		}
		if err := write(ctx, w); err != nil {
			return err
		}
		if r.reject.Load() {
			return errors.New("fixture commit refused")
		}
		return nil
	})
}

func (r *dispatchWaitMutationRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		if r.credentialOutage.Load() {
			return read(dispatchWaitAuthorityReader{Reader: reader, repository: r})
		}
		return read(reader)
	})
}

type dispatchWaitAuthorityReader struct {
	tenantstore.Reader
	repository *dispatchWaitMutationRepository
}

func (r dispatchWaitAuthorityReader) AuthorizeAccess(ctx context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {
	if authority.CredentialID == r.repository.credentialID && r.repository.credentialOutage.Load() {
		return accessstore.Member{}, &accessstore.LookupError{Step: "credential", Cause: tenantauth.ErrUnavailable}
	}
	return r.Reader.AuthorizeAccess(ctx, authority, now)
}

type dispatchWaitCapacityWriter struct{ tenantstore.Writer }

func (w dispatchWaitCapacityWriter) UpdateRunnerCapacityConfig(ctx context.Context, id string, config runnerauth.CapacityConfig) error {
	identity, err := w.RunnerIdentity(ctx, id)
	if err != nil {
		return err
	}
	if identity.CapacityConfig != nil {
		*identity.CapacityConfig = config
	}
	return w.Writer.UpdateRunnerCapacityConfig(ctx, id, config)
}

func (w dispatchWaitCapacityWriter) RunnerClaimState(ctx context.Context, runner string, project tracker.ProjectID, after string, limit int, now time.Time) (runnerstore.ClaimStatePage, error) {
	return w.Writer.(runnerstore.ClaimStateReader).RunnerClaimState(ctx, runner, project, after, limit, now)
}

func TestNativeDispatchWaitLifecycle(t *testing.T) {
	for _, action := range []string{
		"queue", "dependency", "capacity", "unchanged heartbeat", "routing", "revocation", "cancel", "shutdown", "reconnect",
		"replayed queue", "rejected queue", "other tenant queue",
		"capacity local limit", "capacity client limit", "capacity runtime limit", "aliased capacity config", "unchanged capacity config", "stale capacity config", "stale heartbeat", "rejected heartbeat", "heartbeat rollback",
		"unchanged routing", "replayed routing", "interleaved routing replay", "rejected routing", "routing rollback", "other tenant capacity", "other tenant routing", "disabled routing",
		"denied project", "denied runner project", "revoked denied project", "credential outage",
	} {
		t.Run(action, func(t *testing.T) {
			probe := &dispatchWaitPostgresTrace{}
			repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
				config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, probe)
			})
			var clock atomic.Int64
			clock.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1"), now: now})
			organization := nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"}
			seedPostgresHubAdmin(t, repository, service, organization)
			service.database.hostedOrganization = organization.ID
			service.localOrganization = string(organization.ID)
			service.requestMetrics.organization = organization.ID
			f := newNativeFixture(t, service, organization.ID, "wait")
			await := func(count int) {
				testenv.WaitUntil(t, t.Context(), func() error {
					service.notifications.mu.Lock()
					got := len(service.notifications.subscribers[dispatchNotificationKey(organization.ID)])
					service.notifications.mu.Unlock()
					pool := probe.pool.Load()
					if pool != nil && got == count && pool.Stat().AcquiredConns() == 0 {
						return nil
					}
					return fmt.Errorf("subscribers=%d want%d; pool idle=%t", got, count, pool != nil && pool.Stat().AcquiredConns() == 0)
				})
			}
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			worker := r.redemption.Credential
			var identity runnerstore.Identity
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
				var err error
				identity, err = reader.RunnerIdentity(t.Context(), r.binding.RunnerID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			mutations := &dispatchWaitMutationRepository{Repository: repository, credentialID: identity.TokenID, aliasCapacity: action == "aliased capacity config"}
			service.config.TenantRepository = mutations
			config := runnerauth.CapacityConfig{Revision: strings.Repeat("a", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, ObservedAt: now()}
			configCase := strings.Contains(action, "capacity") && action != "capacity" && action != "other tenant capacity"
			if configCase {
				if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
					return w.UpdateRunnerCapacityConfig(t.Context(), r.binding.RunnerID, config)
				}); err != nil {
					t.Fatal(err)
				}
			}
			routingResponse := performHubAPIRequest(t, service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
			requireNativeStatus(t, routingResponse, http.StatusOK)
			var currentRouting runnerauth.Runner
			decodeHubResponse(t, routingResponse, &currentRouting)
			routing := runnerRoutingRequest{RoutingChange: runnerauth.RoutingChange{ExpectedRevision: currentRouting.Revision, Routing: currentRouting.Routing}, IdempotencyKey: "routing-" + action}
			if action == "replayed routing" || action == "interleaved routing replay" {
				routing.Tags = []string{"example"}
			}
			var replayBody string
			if action == "replayed routing" {
				response := performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing)
				requireNativeStatus(t, response, http.StatusOK)
				replayBody = response.Body.String()
			}
			item, blocker := f.create(t, "dependent"), f.create(t, "blocker")
			assertNoExecution := func() {
				t.Helper()
				if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
					active, err := reader.RunnerHasActiveLease(t.Context(), r.binding.RunnerID, now())
					if err != nil {
						return err
					}
					if active {
						return errors.New("cursor reader gained an execution lease")
					}
					owned, err := reader.WorkItem(t.Context(), f.project.ID, item.WorkItemID)
					if err != nil {
						return err
					}
					attempts, err := reader.AttemptPage(t.Context(), owned.ID, leasestore.AttemptCursor{}, 1)
					if err != nil {
						return err
					}
					if len(attempts.Items) != 0 {
						return errors.New("cursor read or refused claim created an attempt")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if action == "routing" || action == "disabled routing" {
				assertNoExecution()
			}
			var other nativeFixture
			var otherRunner runnerFixture
			var otherCursor string
			if strings.HasPrefix(action, "other tenant ") {
				second := nativeOrganization{ID: "org_second", Name: "org_second", Slug: "org-second"}
				seedPostgresHubAdmin(t, repository, service, second)
				other = newNativeFixture(t, service, second.ID, "other wait")
				if action != "other tenant queue" {
					otherRunner = prepareRunner(t, other, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					otherRunner.enroll(t)
				}
				otherCursor = service.notifications.dispatchCursor(dispatchNotificationKey(second.ID))
			}
			var anotherProject nativeFixture
			if action == "denied runner project" {
				anotherProject = newNativeFixture(t, service, organization.ID, "unassigned wait")
			}
			probe.candidates.Store(0)
			if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
				_, err := reader.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project.ID, Scope: string(f.project.ID), WorkItemID: item.WorkItemID, AvailableAt: service.config.now(), Limit: 1})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if probe.candidates.Load() != 1 {
				t.Fatal("PG candidate probe did not observe real candidate owner")
			}
			probe.candidates.Store(0)
			cursor := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, ""))
			if action == "reconnect" {
				f.create(t, "gap")
				if next := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, cursor)); next == cursor {
					t.Fatal("lost reconnect notification")
				}
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan *httptest.ResponseRecorder, 1)
			joined := false
			defer func() {
				cancel()
				if !joined {
					<-result
				}
			}()
			go func() { result <- serveDispatchWait(ctx, f, worker, cursor) }()
			await(1)
			queries := probe.queries.Load()
			if probe.candidates.Load() != 0 {
				t.Fatal("wait scanned candidates")
			}
			quiet := false
			switch action {
			case "queue":
				f.create(t, "ready")
			case "replayed queue":
				if replay := f.create(t, "dependent"); replay.WorkItemID != item.WorkItemID {
					t.Fatal("queue replay changed work item")
				}
			case "rejected queue":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-dependent"}, Title: "different", Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: "Todo"}), http.StatusConflict)
			case "other tenant queue":
				other.create(t, "unrelated queue")
			case "dependency":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "relation"}, ExpectedRevision: item.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
			case "capacity", "unchanged heartbeat", "capacity local limit", "capacity client limit", "capacity runtime limit", "aliased capacity config", "unchanged capacity config", "stale capacity config", "stale heartbeat", "rejected heartbeat", "heartbeat rollback":
				capacity := 2
				if action == "capacity" || action == "heartbeat rollback" {
					capacity = 3
				}
				clock.Add(int64(time.Second))
				if action == "stale heartbeat" || action == "stale capacity config" {
					if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
						if action == "stale capacity config" {
							stale := config
							stale.ObservedAt = now().Add(-runnerauth.HeartbeatTimeout)
							return w.UpdateRunnerCapacityConfig(t.Context(), r.binding.RunnerID, stale)
						}
						identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
						if err != nil {
							return err
						}
						identity.LastHeartbeatAt = now().Add(-runnerauth.HeartbeatTimeout)
						return w.UpdateRunnerIdentity(t.Context(), identity)
					}); err != nil {
						t.Fatal(err)
					}
				}
				heartbeat := map[string]any{"display_name": r.redemption.DisplayName, "capacity": capacity, "version": "test", "backend_isolation": r.redemption.BackendIsolation}
				if configCase {
					switch action {
					case "capacity local limit":
						config.LocalLimit++
					case "capacity client limit":
						config.ClientLimit++
					case "capacity runtime limit", "aliased capacity config":
						config.RuntimeLimit++
					}
					heartbeat["capacity_configuration"] = config
				}
				want := http.StatusOK
				if action == "rejected heartbeat" {
					heartbeat["capacity"] = -1
					want = http.StatusUnprocessableEntity
				}
				if action == "heartbeat rollback" {
					mutations.reject.Store(true)
					want = http.StatusInternalServerError
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", worker, heartbeat), want)
				if err := repository.ReadTenant(t.Context(), organization.ID, corestore.Serving, func(reader tenantstore.Reader) error {
					identity, err := reader.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					if want == http.StatusOK {
						if identity.ReportedCapacity != int64(capacity) || configCase && (identity.CapacityConfig == nil || identity.CapacityConfig.LocalLimit != config.LocalLimit || identity.CapacityConfig.ClientLimit != config.ClientLimit || identity.CapacityConfig.RuntimeLimit != config.RuntimeLimit || !identity.CapacityConfig.ObservedAt.Equal(now())) || !identity.LastHeartbeatAt.Equal(now()) {
							return errors.New("typed heartbeat fields did not commit")
						}
					} else if identity.ReportedCapacity != 2 {
						return errors.New("rejected heartbeat changed durable capacity")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				quiet = action == "unchanged heartbeat" || action == "unchanged capacity config" || want != http.StatusOK

			case "routing", "disabled routing":
				state := "draining"
				if action == "disabled routing" {
					state = "disabled"
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: state, CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}), http.StatusOK)
				refused := performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: r.binding.MachineID, SessionID: "routing-refused", TTLSeconds: 90, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}})
				requireNativeStatus(t, refused, http.StatusNotFound)
				var denial apiErrorResponse
				decodeHubResponse(t, refused, &denial)
				if denial.Code != "machine_not_found" {
					t.Fatalf("claim refusal code=%s", denial.Code)
				}
				assertNoExecution()
			case "unchanged routing", "replayed routing", "interleaved routing replay", "rejected routing", "routing rollback":
				want := http.StatusOK
				if action == "rejected routing" {
					routing.ExpectedRevision = 99
					want = http.StatusConflict
				}
				if action == "routing rollback" {
					routing.Tags = []string{"example"}
					mutations.reject.Store(true)
					want = http.StatusInternalServerError
				}
				var interleave atomic.Bool
				var committedCursor string
				if action == "interleaved routing replay" {
					interleave.Store(true)
					mutations.beforeWrite = func() {
						if interleave.CompareAndSwap(true, false) {
							response := performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing)
							requireNativeStatus(t, response, http.StatusOK)
							replayBody = response.Body.String()
							committedCursor = service.notifications.dispatchCursor(dispatchNotificationKey(organization.ID))
						}
					}
				}
				response := performHubAPIRequest(t, service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, routing)
				requireNativeStatus(t, response, want)
				if (action == "replayed routing" || action == "interleaved routing replay") && response.Body.String() != replayBody {
					t.Fatal("routing replay changed committed response")
				}
				if action == "interleaved routing replay" && (interleave.Load() || service.notifications.dispatchCursor(dispatchNotificationKey(organization.ID)) != committedCursor) {
					t.Fatal("receipt replay notified after another command committed")
				}
				if action == "routing rollback" {
					after := performHubAPIRequest(t, service, http.MethodGet, r.identityPath()+"/routing", testHubAdminToken, nil)
					requireNativeStatus(t, after, http.StatusOK)
					var unchanged runnerauth.Runner
					decodeHubResponse(t, after, &unchanged)
					if unchanged.Revision != currentRouting.Revision || !slices.Equal(unchanged.Tags, currentRouting.Tags) {
						t.Fatal("rejected routing changed durable intent")
					}
				}
				quiet = action != "interleaved routing replay"
			case "other tenant capacity":
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, other.base+"/machines/"+string(otherRunner.binding.MachineID)+"/heartbeat", otherRunner.redemption.Credential, map[string]any{"display_name": otherRunner.redemption.DisplayName, "capacity": 3, "version": "test", "backend_isolation": otherRunner.redemption.BackendIsolation}), http.StatusOK)
				quiet = true
			case "other tenant routing":
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPut, otherRunner.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "draining", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{other.project.ID}}}), http.StatusOK)
				quiet = true
			case "denied project", "denied runner project", "revoked denied project":
				if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
					if action != "denied runner project" {
						return w.DeleteAccessTokenGrant(t.Context(), identity.TokenID, f.project.ID)
					}
					record, err := w.ProjectRecord(t.Context(), f.project.ID)
					if err != nil {
						return err
					}
					return w.RetireProject(t.Context(), f.project.ID, record.Settings.Revision, now())
				}); err != nil {
					t.Fatal(err)
				}
				switch action {
				case "revoked denied project":
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
				case "denied runner project":
					anotherProject.create(t, "reauthorize")
				default:
					f.create(t, "reauthorize")
				}
			case "credential outage":
				mutations.credentialOutage.Store(true)
				service.notifications.notify(dispatchNotificationKey(organization.ID))
			case "revocation":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
			case "cancel":
				cancel()
			case "shutdown":
				if err := f.service.Shutdown(testenv.WaitContext(t, t.Context())); err != nil {
					t.Fatal(err)
				}
			}
			quiet = quiet || action == "replayed queue" || action == "rejected queue" || action == "other tenant queue"
			if strings.HasPrefix(action, "other tenant ") && service.notifications.dispatchCursor(dispatchNotificationKey(other.project.OrganizationID)) == otherCursor {
				t.Fatal("other tenant committed mutation did not notify its own scope")
			}
			if quiet {
				if next := service.notifications.dispatchCursor(dispatchNotificationKey(f.project.OrganizationID)); next != cursor {
					t.Fatal("unchanged/replayed/rejected/other-tenant command woke admission")
				}
				cancel()
			} else if action != "cancel" && action != "unchanged heartbeat" && action != "shutdown" {
				if next := service.notifications.dispatchCursor(dispatchNotificationKey(f.project.OrganizationID)); next == cursor {
					t.Fatal("notification did not advance cursor after committed mutation")
				}
			}
			select {
			case response := <-result:
				joined = true
				switch {
				case quiet || action == "cancel":
				case action == "shutdown":
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
				case action == "revocation" || action == "revoked denied project":
					requireNativeStatus(t, response, http.StatusUnauthorized)
				case action == "denied project" || action == "denied runner project":
					requireNativeStatus(t, response, http.StatusForbidden)
				case action == "credential outage":
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
				default:
					if next := dispatchWaitCursor(t, response); next == cursor {
						t.Fatal("notification did not advance cursor")
					}
				}
			case <-testenv.WaitContext(t, t.Context()).Done():
				t.Fatal("held wait did not finish")
			}
			await(0)
			t.Logf("queries=%d candidate_queries=%d", probe.queries.Load()-queries, probe.candidates.Load())
		})
	}
}

func TestNativeDispatchWaitScale(t *testing.T) {
	for _, runners := range []int{1, 16, 64, 256} {
		for _, admissions := range []int{1, 8} {
			if admissions > runners {
				continue
			}
			t.Run(fmt.Sprintf("runners_%d_admissions_%d", runners, admissions), func(t *testing.T) {
				f := newNativeFixture(t, nil, "", "scale")
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker := f.worker(t, "scale-worker")
				for index := range runners {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": fmt.Sprintf("machine-%d", index), "hostname": "isolated", "capacity": 1, "version": "test"}), http.StatusOK)
				}
				probe := measureDispatchQueries(t, f.service)
				cursor := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, ""))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				results := make(chan time.Duration, runners)
				failures := make(chan int, runners)
				var began atomic.Int64
				var memoryBefore, memoryHeld runtime.MemStats
				runtime.ReadMemStats(&memoryBefore)
				requestsBefore := probe.queries.Load()
				for index := range runners {
					go func() {
						response := serveDispatchWait(ctx, f, worker, cursor)
						if response.Code != http.StatusOK {
							failures <- response.Code
							results <- 0
							return
						}
						claim := tracker.NativeClaim{MachineID: tracker.MachineID(fmt.Sprintf("machine-%d", index)), SessionID: fmt.Sprintf("session-%d", index), PolicyID: hubTestPolicy().ID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}}
						claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
						if claimed.Code == http.StatusOK {
							results <- time.Since(time.Unix(0, began.Load()))
							return
						}
						if claimed.Code != http.StatusConflict {
							failures <- claimed.Code
						}
						results <- 0
					}()
				}
				awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, runners)
				runtime.ReadMemStats(&memoryHeld)
				idleReads := probe.queries.Load()
				idlePool := f.service.database.db.Stats()
				time.Sleep(50 * time.Millisecond)
				if probe.queries.Load() != idleReads || probe.candidates.Load() != 0 {
					t.Fatal("held waits performed database work")
				}
				began.Store(time.Now().UnixNano())
				credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
				_, err = f.service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "dispatch-scale"}, tracker.Mutation{IdempotencyKey: "burst"}, admissions, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
					var items []tracker.NativeIssue
					for index := range admissions {
						item, err := createNativeFixtureIssueTx(ctx, tx, scope, tracker.CreateIssue{Title: fmt.Sprintf("ready-%d", index), Body: issueContractTestSections, State: "Todo"}, now)
						if err != nil {
							return nil, err
						}
						items = append(items, item)
					}
					return items, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for range 8 {
					f.service.notifications.notify(dispatchNotificationKey(f.project.OrganizationID))
				}
				granted := 0
				var worst time.Duration
				settleCtx := testenv.WaitContext(t, t.Context())
				for range runners {
					select {
					case latency := <-results:
						if latency > 0 {
							granted++
							worst = max(worst, latency)
						}
					case <-settleCtx.Done():
						t.Fatal("scale request did not settle")
					}
				}
				if len(failures) > 0 {
					t.Fatalf("unexpected HTTP status %d", <-failures)
				}
				var leases, issues int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*), count(DISTINCT issue_id) FROM leases WHERE released_at IS NULL").Scan(&leases, &issues); err != nil {
					t.Fatal(err)
				}
				if granted != admissions || leases != admissions || issues != admissions {
					t.Fatalf("grants=%d leases=%d unique=%d want=%d", granted, leases, issues, admissions)
				}
				pool := f.service.database.db.Stats()
				t.Logf("wait_requests=%d claim_requests=%d entry_queries=%d idle_queries=0 idle_candidates=0 held_heap_bytes=%d wait_entry_alloc_bytes=%d total_queries=%d candidate_queries=%d candidate_rows=%d idle_pool_wait=%s total_pool_wait=%s wake_to_claim_max=%s", runners, runners, idleReads-requestsBefore, memoryHeld.HeapAlloc, memoryHeld.TotalAlloc-memoryBefore.TotalAlloc, probe.queries.Load()-requestsBefore, probe.candidates.Load(), probe.rows.Load(), idlePool.WaitDuration, pool.WaitDuration, worst)
			})
		}
	}
}

func TestNativeCandidateQueryBound(t *testing.T) {
	var bindBytes int64
	for _, test := range []struct {
		size        int
		unpublished bool
		filtered    bool
	}{{size: 16}, {size: 256}, {size: 2048}, {size: 256, unpublished: true}, {size: 16, filtered: true}, {size: 256, filtered: true}, {size: 2048, filtered: true}} {
		t.Run(fmt.Sprintf("queue_%d_unpublished_%t_filtered_%t", test.size, test.unpublished, test.filtered), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "candidate-bound")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var tail tracker.NativeIssue
			var issues []tracker.NativeIssue
			for index := range test.size {
				request := tracker.CreateIssue{Title: fmt.Sprintf("candidate-%d", index), Body: issueContractTestSections, State: "Todo"}
				if test.filtered {
					request.Labels, request.Assignees = []string{"skip"}, []string{"other"}
				}
				if index == test.size-1 {
					request.Priority = new(1)
					request.Labels = []string{"hotfix"}
					if test.filtered {
						request.Priority = nil
						request.Labels, request.Assignees = []string{" Required ", "second"}, []string{" Chosen "}
					}
				}
				tail, err = createNativeFixtureIssueTx(t.Context(), tx, scope, request, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				issues = append(issues, tail)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if test.unpublished {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker := f.worker(t, "source-worker")
				lease := claimNativeAttempt(t, f, worker, "source-machine", "source-session", tail.WorkItemID)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(tail.WorkItemID) + "/events"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
				event.Data.Handoff = nativeTestCheckpoint()
				event.Data.Handoff.WorktreeState = "unpushed"
				event.Data.Handoff.HeadSHA = strings.Repeat("b", 40)
				event.Data.Handoff.WorkspaceDigest = strings.Repeat("d", 64)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.finished", "finish", 3
				event.Data.Handoff, event.Data.Outcome = nil, "succeeded"
				event.Data.CompletionBody = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				var payloadKey string
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT payload_object_key FROM native_attempts WHERE work_item_id = ?", tail.WorkItemID).Scan(&payloadKey); err != nil {
					t.Fatal(err)
				}
				inline, err := readStoredPayload(f.service.contentContext(t.Context()), payloadKey)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json = ?, payload_object_key = '' WHERE work_item_id = ?", string(inline), tail.WorkItemID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json = json_remove(data_json, '$.disposition') WHERE work_item_id = ?", tail.WorkItemID); err != nil {
					t.Fatal(err)
				}
				for _, issue := range issues[:len(issues)-1] {
					leaseID := newNativeID("lease")
					result, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
(lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
SELECT ?, i.id, l.machine_id, ?, l.expires_at, l.acquired_at, l.renewed_at, l.released_at, l.created_at, l.updated_at
FROM leases l JOIN issues i ON i.native_id = ? WHERE l.lease_id = ?`, leaseID, newNativeID("session"), issue.WorkItemID, lease.ID)
					if err != nil {
						t.Fatal(err)
					}
					fence, err := result.LastInsertId()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
(id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, checkpoint_json, started_at, updated_at, work_item_revision, dispatch_generation)
SELECT ?, a.organization_id, a.project_id, i.native_id, ?, ?, ?, a.sequence, a.status, a.data_json, a.checkpoint_json, a.started_at, a.updated_at, i.revision, i.dispatch_generation
FROM native_attempts a JOIN issues i ON i.native_id = ? WHERE a.work_item_id = ?`, newNativeID("attempt"), leaseID, fence, newNativeID("run"), issue.WorkItemID, tail.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE leases SET released_at = NULL, expires_at = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)`, formatHubTime(f.service.config.now().Add(time.Hour)), issues[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			probe := measureDispatchQueries(t, f.service)
			tx, err = f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var tailID tracker.WorkItemID
			if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", tail.WorkItemID).Scan(&tailID); err != nil {
				t.Fatal(err)
			}
			if test.filtered {
				for _, filter := range []struct {
					name                          string
					assignees, included, excluded []string
					absent                        bool
				}{
					{name: "assignee", assignees: []string{"chosen"}},
					{name: "included labels", included: []string{"required", "second"}},
					{name: "excluded label", excluded: []string{"skip"}},
					{name: "combined", assignees: []string{"chosen"}, included: []string{"required", "second"}, excluded: []string{"skip"}},
					{name: "missing assignee", assignees: []string{"absent"}, absent: true},
					{name: "missing label", included: []string{"absent"}, absent: true},
					{name: "all labels required", included: []string{"required", "absent"}, absent: true},
				} {
					t.Run(filter.name, func(t *testing.T) {
						probe.candidates.Store(0)
						probe.rows.Store(0)
						query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), Limit: 1}
						ids, err := nativeCandidateIDs(t.Context(), tx, query, nil, nil, nil, nil, filter.assignees, filter.included, filter.excluded)
						if err != nil {
							t.Fatal(err)
						}
						want := []tracker.WorkItemID{tailID}
						if filter.absent {
							want = nil
						}
						if !slices.Equal(ids, want) || probe.candidates.Load() != 1 || probe.rows.Load() != int64(len(want)) {
							t.Fatalf("ids=%v want=%v candidate_queries=%d returned_rows=%d", ids, want, probe.candidates.Load(), probe.rows.Load())
						}
					})
				}
				return
			}
			query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), Limit: 9, AvailableAt: f.service.config.now().Truncate(time.Second), DispatchPriorityByState: []string{"Todo"}, DispatchPriorityByLabel: []string{"hotfix"}}
			began := time.Now()
			ids, err := claimCandidateIDs(f.service.runDetailContext(t.Context()), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(began)
			wantRows := int64(9)
			if len(ids) != 9 || ids[0] != tailID || probe.rows.Load() != wantRows {
				t.Fatalf("ids=%v tail=%d returned_rows=%d", ids, tailID, probe.rows.Load())
			}
			if bindBytes == 0 {
				bindBytes = probe.maxArgumentBytes.Load()
			}
			if probe.maxArgumentBytes.Load() != bindBytes {
				t.Fatalf("bind payload grew with candidate cohort: got=%d want=%d", probe.maxArgumentBytes.Load(), bindBytes)
			}
			if test.unpublished {
				if probe.completionBodies.Load() != 9 {
					t.Fatalf("first page completion_bodies=%d bind_bytes=%d", probe.completionBodies.Load(), probe.maxArgumentBytes.Load())
				}
				seen := slices.Clone(ids)
				pages := 1
				for len(ids) == query.Limit {
					query.After = ids[len(ids)-1]
					before := probe.completionBodies.Load()
					ids, err = claimCandidateIDs(f.service.runDetailContext(t.Context()), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					if work := probe.completionBodies.Load() - before; work != int64(len(ids)+1) {
						t.Fatalf("page=%d bodies=%d want=%d including cursor", pages, work, len(ids)+1)
					}
					for _, id := range ids {
						if slices.Contains(seen, id) {
							t.Fatalf("page repeated candidate %d", id)
						}
					}
					seen = append(seen, ids...)
					pages++
				}
				var leasedID tracker.WorkItemID
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", issues[0].WorkItemID).Scan(&leasedID); err != nil {
					t.Fatal(err)
				}
				if len(seen) != test.size-1 || slices.Contains(seen, leasedID) || probe.maxArgumentBytes.Load() != bindBytes {
					t.Fatalf("paged=%d leased=%d bind_bytes=%d", len(seen), leasedID, probe.maxArgumentBytes.Load())
				}
				t.Logf("pages=%d candidates=%d legacy_bodies=%d max_bind_bytes=%d", pages, len(seen), probe.completionBodies.Load(), probe.maxArgumentBytes.Load())
				for _, issue := range issues[:200] {
					body := "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve scope\n```"
					if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.completion_body', ?) WHERE work_item_id = ?`, body, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = CASE WHEN issue_id = ? THEN 1 ELSE COALESCE(priority_override, 2) END", tailID); err != nil {
					t.Fatal(err)
				}
				query.After, query.Limit = 0, 1
				before := probe.completionBodies.Load()
				ids, err = claimCandidateIDs(f.service.runDetailContext(t.Context()), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
				if err != nil || !slices.Equal(ids, []tracker.WorkItemID{tailID}) || probe.completionBodies.Load()-before != 200 {
					t.Fatalf("after nonqualifying reports ids=%v bodies=%d err=%v", ids, probe.completionBodies.Load()-before, err)
				}
				t.Logf("claim-sized selection examined %d reports before ranked qualifying candidate", probe.completionBodies.Load()-before)
			}
			captured := probe.query.Load()
			plan, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+captured.statement, captured.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, "SEARCH i ") || strings.Contains(detail, "SEARCH candidate ") || strings.Contains(detail, "ORDER BY") {
					t.Log(detail)
				}
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			if err := plan.Close(); err != nil {
				t.Fatal(err)
			}
			query.After = ids[0]
			if test.unpublished {
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.completion_body', ?) WHERE work_item_id = ?`, "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve scope\n```", tail.WorkItemID); err != nil {
					t.Fatal(err)
				}
				before := probe.completionBodies.Load()
				if _, err := claimCandidateIDs(f.service.runDetailContext(t.Context()), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil); !isProviderWait(err) || probe.completionBodies.Load()-before != 1 {
					t.Fatalf("answered cursor must refresh without reading next page: bodies=%d err=%v", probe.completionBodies.Load()-before, err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET archived = 1 WHERE id = ?", query.After); err != nil {
				t.Fatal(err)
			}
			if _, err := claimCandidateIDs(f.service.runDetailContext(t.Context()), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil); err == nil {
				t.Fatal("removed cursor did not request a fresh preview")
			}
			t.Logf("queue=%d page_rows=9 select_duration=%s", test.size, elapsed)
		})
	}
}
