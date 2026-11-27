package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dispatchProbedQuery struct {
	statement string
	args      []any
}

type dispatchCandidateQueryKey struct{}

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

func awaitDispatchSubscribers(t *testing.T, s *Service, organization tracker.OrganizationID, count int, probe *dispatchWaitPostgresTrace) {
	t.Helper()
	testenv.WaitUntil(t, t.Context(), func() error {
		got := s.notifications.Subscribers(dispatchNotificationKey(organization))
		s.tokenUseWork.Wait()
		pool := probe.pool.Load()
		if got == count && pool != nil && pool.Stat().AcquiredConns() == 0 {
			return nil
		}
		return fmt.Errorf("subscribers=%d, want %d", got, count)
	})
}

type dispatchWaitPostgresTrace struct {
	queries          atomic.Int64
	candidates       atomic.Int64
	pool             atomic.Pointer[pgxpool.Pool]
	rows             atomic.Int64
	query            atomic.Pointer[dispatchProbedQuery]
	maxArgumentBytes atomic.Int64
	details          *dispatchWaitDetails
}

func (p *dispatchWaitPostgresTrace) record(statement string) {
	p.queries.Add(1)
	if strings.HasPrefix(strings.TrimSpace(statement), "WITH candidates AS (") {
		p.candidates.Add(1)
	}
}
func (p *dispatchWaitPostgresTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	p.record(data.SQL)
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH candidates AS (") {
		p.capture(data.SQL, data.Args)
		return context.WithValue(ctx, dispatchCandidateQueryKey{}, true)
	}
	return ctx
}
func (p *dispatchWaitPostgresTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(dispatchCandidateQueryKey{}) == true && data.Err == nil {
		p.rows.Add(data.CommandTag.RowsAffected())
	}
}
func (p *dispatchWaitPostgresTrace) capture(statement string, args []any) {
	values := args
	if len(args) == 1 {
		if named, ok := args[0].(pgx.NamedArgs); ok {
			values = slices.Collect(maps.Values(named))
		}
	}
	var argumentBytes int64
	for _, arg := range values {
		switch value := arg.(type) {
		case string:
			argumentBytes += int64(len(value))
		case []string:
			for _, text := range value {
				argumentBytes += int64(len(text))
			}
		default:
			reflected := reflect.ValueOf(arg)
			if reflected.IsValid() && reflected.Kind() == reflect.String {
				argumentBytes += int64(reflected.Len())
			}
		}
	}
	for old := p.maxArgumentBytes.Load(); argumentBytes > old; old = p.maxArgumentBytes.Load() {
		if p.maxArgumentBytes.CompareAndSwap(old, argumentBytes) {
			break
		}
	}
	p.query.Store(&dispatchProbedQuery{statement: statement, args: slices.Clone(args)})
}
func (p *dispatchWaitPostgresTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, q := range data.Batch.QueuedQueries {
		p.record(q.SQL)
	}
	return ctx
}
func (p *dispatchWaitPostgresTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "WITH candidates AS (") {
		p.capture(data.SQL, data.Args)
		if data.Err == nil {
			p.rows.Add(data.CommandTag.RowsAffected())
		}
	}
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
	t.Parallel()
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
			service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1"), Clock: now})
			organization := nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"}
			seedPostgresHubAdmin(t, repository, service, organization)
			service.database.hostedOrganization = organization.ID
			service.localOrganization = string(organization.ID)
			service.requestMetrics.Organization = organization.ID
			f := newNativeFixture(t, service, organization.ID, "wait")
			await := func(count int) {
				testenv.WaitUntil(t, t.Context(), func() error {
					got := service.notifications.Subscribers(dispatchNotificationKey(organization.ID))
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
				_, err := reader.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: f.project.ID, Scope: string(f.project.ID), WorkItemID: item.WorkItemID, AvailableAt: service.config.Clock(), Limit: 1})
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
				requireNativeError(t, refused, http.StatusConflict, "runner_"+state)
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
				case action == "denied project":
					requireNativeStatus(t, response, http.StatusNotFound)
				case action == "denied runner project":
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

type dispatchWaitDetails struct {
	*verifiedAttemptDetails
	attemptKeys sync.Map
	reads       atomic.Int64
}

func (d *dispatchWaitDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if _, found := d.attemptKeys.Load(ref.Key); found {
		d.reads.Add(1)
	}
	return d.verifiedAttemptDetails.ReadDetail(ctx, organization, ref)
}

func prepareDispatchIssuesFixture(t *testing.T, f nativeFixture, requests []tracker.CreateIssue) (func(), []tracker.NativeIssue) {
	t.Helper()
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority status%d: %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	var project projectstore.Record
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		project, err = r.ProjectRecord(t.Context(), scope.Project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	type preparedIssue struct {
		plan    hubrecords.WorkItemRecordPlan
		fields  hubrecords.WorkItemRecordFields
		version eventstore.CollaborationVersion
		event   eventstore.Event
	}
	prepared := make([]preparedIssue, 0, len(requests))
	issues := make([]tracker.NativeIssue, 0, len(requests))
	content := eventstore.NewContent(f.service.runDetailStore())
	for index, request := range requests {
		plan, err := f.service.planWorkItemRecord(t.Context(), scope, hubrecords.WorkItemRecordPlan{Project: project, Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{Number: index + 1}}}, request)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := f.service.prepareWorkItemFields(t.Context(), scope.Organization, plan)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(plan.Issue)
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: scope.Organization, ProjectID: scope.Project, WorkItemID: plan.Issue.WorkItemID, RecordID: string(plan.Issue.WorkItemID), Revision: int64(plan.Issue.Revision)}, raw)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: hubapi.NewID("evt"), OrganizationID: scope.Organization, ProjectID: scope.Project, AggregateID: plan.Issue.WorkItemID, AggregateType: "issue", Type: "issue.created", SchemaVersion: 1, Actor: plan.Issue.Actor, RecordedAt: plan.Issue.CreatedAt, Data: tracker.CollaborationData{Revision: plan.Issue.Revision, ToState: plan.Issue.State}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), scope.Organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		prepared = append(prepared, preparedIssue{plan: plan, fields: fields, version: version, event: event})
		issues = append(issues, plan.Issue)
	}
	return func() {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, item := range prepared {
				if err := hubrecords.WriteWorkItemRecord(t.Context(), w, item.plan, item.fields); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), item.version); err != nil {
					return err
				}
				if _, err := w.AppendEvent(t.Context(), item.event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.service.signalNativeDispatch(scope)
	}, issues
}

func newDispatchWaitFixture(t *testing.T, name string) (nativeFixture, *dispatchWaitPostgresTrace) {
	t.Helper()
	probe := &dispatchWaitPostgresTrace{details: &dispatchWaitDetails{verifiedAttemptDetails: &verifiedAttemptDetails{}}}
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, probe)
	})
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := openLocalPostgresHubTestService(t, repository, Config{SecretKeys: secretTestKeys(t, "1", "1"), RunDetails: probe.details, Clock: func() time.Time { return now }})
	organization := nativeOrganization{ID: "org_first", Name: "org_first", Slug: "org-first"}
	seedPostgresHubAdmin(t, repository, service, organization)
	service.database.hostedOrganization = organization.ID
	service.localOrganization = string(organization.ID)
	service.requestMetrics.Organization = organization.ID
	return newNativeFixture(t, service, organization.ID, name), probe
}

func TestNativeDispatchWaitGrantsDistinctWork(t *testing.T) {
	t.Parallel()
	for _, runners := range []int{1, 4} {
		for _, admissions := range []int{1, 2} {
			if admissions > runners {
				continue
			}
			t.Run(fmt.Sprintf("runners_%d_admissions_%d", runners, admissions), func(t *testing.T) {
				t.Parallel()
				f, probe := newDispatchWaitFixture(t, "concurrent-dispatch")
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				workers := make([]runnerFixture, runners)
				cursors := make([]string, runners)
				for index := range runners {
					worker := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
					worker.redemption.Capacity = 1
					worker.enroll(t)
					reportRunnerCapacity(t, f.service, f.base, worker.redemption.Credential, worker.binding.MachineID, "Example runner", isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, 1)
					workers[index] = worker
				}
				for index, worker := range workers {
					cursors[index] = dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker.redemption.Credential, ""))
				}
				requests := make([]tracker.CreateIssue, admissions)
				for index := range admissions {
					requests[index] = tracker.CreateIssue{Title: fmt.Sprintf("ready-%d", index), Body: issueContractTestSections, State: "Todo"}
				}
				publish, _ := prepareDispatchIssuesFixture(t, f, requests)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				results := make(chan bool, runners)
				failures := make(chan int, runners)
				var joined sync.WaitGroup
				defer func() { cancel(); joined.Wait() }()
				for index := range runners {
					joined.Add(1)
					go func() {
						defer joined.Done()
						worker := workers[index]
						response := serveDispatchWait(ctx, f, worker.redemption.Credential, cursors[index])
						if response.Code != http.StatusOK {
							failures <- response.Code
							results <- false
							return
						}
						claim := tracker.NativeClaim{MachineID: worker.binding.MachineID, SessionID: fmt.Sprintf("session-%d", index), PolicyID: hubTestPolicy().ID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}}
						raw, err := json.Marshal(claim)
						if err != nil {
							failures <- http.StatusInternalServerError
							results <- false
							return
						}
						request := httptest.NewRequest(http.MethodPost, f.base+"/claims", bytes.NewReader(raw)).WithContext(ctx)
						request.Header.Set("Authorization", "Bearer "+worker.redemption.Credential)
						request.Header.Set("Content-Type", "application/json")
						claimed := httptest.NewRecorder()
						f.service.echo.ServeHTTP(claimed, request)
						if claimed.Code == http.StatusOK {
							results <- true
							return
						}
						if claimed.Code != http.StatusConflict {
							failures <- claimed.Code
						}
						results <- false
					}()
				}
				awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, runners, probe)
				publish()

				for range 8 {
					f.service.notifications.notify(dispatchNotificationKey(f.project.OrganizationID))
				}
				granted := 0
				settleCtx := testenv.WaitContext(t, t.Context())
				for range runners {
					select {
					case grantedClaim := <-results:
						if grantedClaim {
							granted++
						}
					case <-settleCtx.Done():
						t.Fatal("dispatch request did not settle")
					}
				}
				if len(failures) > 0 {
					t.Fatalf("unexpected HTTP status %d", <-failures)
				}
				var leases, issues int
				if err := conversationTestAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*),count(DISTINCT issue_id) FROM leases WHERE organization_id=$1 AND project_id=$2 AND released_at IS NULL", f.project.OrganizationID, f.project.ID).Scan(&leases, &issues); err != nil {
					t.Fatal(err)
				}
				if granted != admissions || leases != admissions || issues != admissions {
					t.Fatalf("grants=%d leases=%d unique=%d want=%d", granted, leases, issues, admissions)
				}
				joined.Wait()
				awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, 0, probe)
			})
		}
	}
}

func TestNativeCandidateFiltersAndCursor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		size        int
		unpublished bool
		filtered    bool
	}{{size: 16}, {size: 256, unpublished: true}, {size: 16, filtered: true}} {
		t.Run(fmt.Sprintf("queue_%d_unpublished_%t_filtered_%t", test.size, test.unpublished, test.filtered), func(t *testing.T) {
			t.Parallel()
			f, probe := newDispatchWaitFixture(t, "candidate-bound")
			requests := make([]tracker.CreateIssue, test.size)
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
				requests[index] = request
			}
			publish, issues := prepareDispatchIssuesFixture(t, f, requests)
			publish()
			tail := issues[len(issues)-1]
			var tailRow, firstRow corestore.WorkItem
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				tailRow, err = r.WorkItem(t.Context(), f.project.ID, tail.WorkItemID)
				if err != nil {
					return err
				}
				firstRow, err = r.WorkItem(t.Context(), f.project.ID, issues[0].WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			tailID := tracker.WorkItemID(tailRow.ID)
			attempts := map[tracker.NativeWorkItemID]dispatchGuardFixture{}
			var worker runnerFixture
			var scope hubscope.NativeScope
			if test.unpublished {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker = prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
				worker.redemption.Capacity = test.size
				worker.enroll(t)
				admin, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("fixture admin status%d: %v", status, err)
				}
				credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker.redemption.Credential), "", "")
				if err != nil || status != http.StatusOK {
					t.Fatalf("fixture worker status%d: %v", status, err)
				}
				scope = hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
				for _, issue := range issues {
					lease := nativeAttemptFixtureLease(t, worker, issue, hubapi.NewID("source-session"))
					event := nativeStartedEvent(lease)
					path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker.redemption.Credential, event), http.StatusOK)
					event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
					event.Data.Handoff = nativeTestCheckpoint()
					event.Data.Handoff.WorktreeState = "unpushed"
					event.Data.Handoff.HeadSHA = strings.Repeat("b", 40)
					event.Data.Handoff.WorkspaceDigest = strings.Repeat("d", 64)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker.redemption.Credential, event), http.StatusOK)
					event.Type, event.IdempotencyKey, event.Data.Sequence = "run.finished", "finish", 3
					event.Data.Handoff, event.Data.Outcome = nil, "succeeded"
					event.Data.CompletionBody = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker.redemption.Credential, event), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
					attempt := dispatchGuardFixture{conversationWorkerFixture: &conversationWorkerFixture{nativeFixture: f, worker: worker.redemption.Credential, issue: issue, lease: lease, attempt: event.Data.AttemptID, run: event.Data.RunID, policy: lease.PolicyID}, authority: admin}
					attempts[issue.WorkItemID] = attempt
					snapshot, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), attempt.scope(), string(issue.WorkItemID), event.Data.AttemptID)
					if err != nil {
						t.Fatal(err)
					}
					probe.details.attemptKeys.Store(snapshot.Attempt.PayloadObjectKey, true)
				}
				held := nativeAttemptFixtureLease(t, worker, issues[0], "held-source-session")
				if held.WorkItemID != issues[0].WorkItemID {
					t.Fatal("first source item did not retain an active lease")
				}
				example := attempts[tail.WorkItemID]
				snapshot, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), example.scope(), string(tail.WorkItemID), example.attempt)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.readNativeAttemptRunTenant(t.Context(), snapshot); err != nil {
					t.Fatal(err)
				}
				if probe.details.reads.Load() == 0 {
					t.Fatal("attempt source read tracer was not exercised")
				}
			}
			query := claimstore.CandidateQuery{ProjectID: f.project.ID, Scope: string(f.project.ID), AvailableAt: f.service.config.Clock().Truncate(time.Second), States: []string{"todo"}, Limit: 9, StatePriority: []string{"Todo"}, LabelPriority: []string{"hotfix"}}
			read := func(query claimstore.CandidateQuery) (claimstore.CandidatePage, error) {
				var page claimstore.CandidatePage
				err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					page, err = r.CandidatePage(t.Context(), query)
					return err
				})
				return page, err
			}
			idsOf := func(page claimstore.CandidatePage) []tracker.WorkItemID {
				var ids []tracker.WorkItemID
				for _, item := range page.Items {
					ids = append(ids, tracker.WorkItemID(item.ID))
				}
				return ids
			}
			probe.candidates.Store(0)
			probe.rows.Store(0)
			probe.maxArgumentBytes.Store(0)
			probe.details.reads.Store(0)
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
						filtered := query
						filtered.Limit = 1
						filtered.Assignees = filter.assignees
						filtered.IncludeLabels = filter.included
						filtered.ExcludeLabels = filter.excluded
						page, err := read(filtered)
						if err != nil {
							t.Fatal(err)
						}
						ids := idsOf(page)
						want := []tracker.WorkItemID{tailID}
						if filter.absent {
							want = nil
						}
						if !slices.Equal(ids, want) {
							t.Fatalf("ids=%v want=%v", ids, want)
						}
					})
				}
				return
			}
			page, err := read(query)
			if err != nil {
				t.Fatal(err)
			}
			ids := idsOf(page)
			if len(ids) != 9 || ids[0] != tailID {
				t.Fatalf("ids=%v tail=%d", ids, tailID)
			}
			if test.unpublished {
				if probe.details.reads.Load() != 0 {
					t.Fatalf("typed first page read %d completion payloads", probe.details.reads.Load())
				}
				seen := slices.Clone(ids)
				for len(ids) == query.Limit {
					query.AfterWorkItemID = page.Items[len(page.Items)-1].WorkItemID
					page, err = read(query)
					if err != nil {
						t.Fatal(err)
					}
					ids = idsOf(page)
					for _, id := range ids {
						if slices.Contains(seen, id) {
							t.Fatalf("page repeated candidate %d", id)
						}
					}
					seen = append(seen, ids...)
				}
				if len(seen) != test.size-1 || slices.Contains(seen, tracker.WorkItemID(firstRow.ID)) || probe.details.reads.Load() != 0 {
					t.Fatalf("paged=%d leased=%d completion_payloads=%d", len(seen), firstRow.ID, probe.details.reads.Load())
				}
				for _, issue := range issues[:200] {
					rewriteDispatchAttemptFixture(t, attempts[issue.WorkItemID], func(run *tracker.NativeRunData) {
						run.CompletionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve scope\n```"
					})
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					for index, issue := range issues {
						entry, err := w.QueueEntry(t.Context(), f.project.ID, issue.WorkItemID, string(f.project.ID))
						if err != nil {
							return err
						}
						priority := int64(2)
						if index < 200 {
							priority = 0
						}
						if issue.WorkItemID == tail.WorkItemID {
							priority = 1
						}
						entry.Priority = &priority
						if err := w.PutQueueEntry(t.Context(), entry); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				reportRunnerCapacity(t, f.service, f.base, worker.redemption.Credential, worker.binding.MachineID, worker.redemption.DisplayName, worker.redemption.BackendIsolation, worker.redemption.Capacity)
				claim := tracker.NativeClaim{MachineID: worker.binding.MachineID, SessionID: "ranked-tail", PolicyID: hubTestPolicy().ID, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}, WorkflowStates: []string{"todo"}}
				selected, err := f.service.claimNativeIssueTenant(t.Context(), scope, claim, 90*time.Second)
				if err != nil || selected.IssueID != tailRow.ID {
					t.Errorf("after nonqualifying reports selected=%d tail=%d err=%v", selected.IssueID, tailRow.ID, err)
				}
				if selected.LeaseID != "" {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+selected.LeaseID+"/release", worker.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(selected.FencingToken), Reason: "completed"}), http.StatusNoContent)
				}
				rewriteDispatchAttemptFixture(t, attempts[tail.WorkItemID], func(run *tracker.NativeRunData) {
					run.CompletionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Approve scope\n```"
				})
				claim.SessionID = "ranked-preview"
				_, err = f.service.previewProviderCandidatesTenant(t.Context(), scope, tracker.NativeCapacityPreview{NativeClaim: claim, After: tailID, Limit: 9}, f.service.config.Clock())
				if !isProviderWait(err) {
					t.Errorf("answered cursor must refresh: %v", err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(tail.WorkItemID)+"/archive", f.token, archiveRequest(tail, "removed-cursor")), http.StatusOK)
			query.AfterWorkItemID = tail.WorkItemID
			if _, err := read(query); err == nil {
				t.Fatal("removed cursor did not request a fresh preview")
			}
		})
	}
}

func TestNativeDispatchPreviewCapacity(t *testing.T) {
	f, probe := newDispatchWaitFixture(t, "dispatch discovery")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
	runner.redemption.Capacity = 1
	runner.enroll(t)
	runner.reportCapacity(t)
	token := runner.redemption.Credential
	claim := tracker.NativeClaim{
		MachineID: runner.binding.MachineID, SessionID: "example-session", PolicyID: hubTestPolicy().ID,
		TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		WorkflowStates: []string{"Todo"},
	}
	call := func(name string, routes ...func() *httptest.ResponseRecorder) tracker.NativeCapacityPage {
		t.Helper()
		f.service.tokenUseWork.Wait()
		before := probe.queries.Load()
		payload := 0
		var response *httptest.ResponseRecorder
		for _, route := range routes {
			response = route()
			requireNativeStatus(t, response, http.StatusOK)
			payload += response.Body.Len()
		}
		f.service.tokenUseWork.Wait()
		t.Logf("%s: discovery requests=%d PostgreSQL statements=%d response bytes=%d", name, len(routes), probe.queries.Load()-before, payload)
		var page tracker.NativeCapacityPage
		decodeHubResponse(t, response, &page)
		return page
	}
	preview := func(checkCapacity bool, claim tracker.NativeClaim) func() *httptest.ResponseRecorder {
		return func() *httptest.ResponseRecorder {
			return performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", token, tracker.NativeCapacityPreview{NativeClaim: claim, Limit: 3, CheckCapacity: checkCapacity})
		}
	}
	legacy := call("legacy idle", func() *httptest.ResponseRecorder {
		return performHubAPIRequest(t, f.service, http.MethodGet, runner.identityPath()+"/routing", token, nil)
	}, preview(false, claim))
	expect := func(page tracker.NativeCapacityPage, full bool, items ...tracker.NativeWorkItemID) {
		t.Helper()
		var ids []tracker.NativeWorkItemID
		for _, item := range page.Items {
			ids = append(ids, item.WorkItemID)
		}
		if page.CapacityFull != full || page.Next != 0 || !slices.Equal(ids, items) {
			t.Fatalf("preview: full=%t next=%d items=%v, want full=%t items=%v", page.CapacityFull, page.Next, ids, full, items)
		}
	}
	expect(legacy, false)
	expect(call("combined idle", preview(true, claim)), false)
	expect(call("combined idle repeat", preview(true, claim)), false)

	wrongMachine := claim
	wrongMachine.MachineID = runnerauth.NewBinding().MachineID
	requireNativeStatus(t, preview(true, wrongMachine)(), http.StatusNotFound)

	cursor := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, token, ""))
	ctx, cancel := context.WithCancel(t.Context())
	waited := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		waited <- serveDispatchWait(ctx, f, token, cursor)
	}()
	defer func() { cancel(); <-done }()
	awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, 1, probe)
	item := f.create(t, "newly eligible work")
	select {
	case response := <-waited:
		if dispatchWaitCursor(t, response) == cursor {
			t.Fatal("work filing did not advance the dispatch cursor")
		}
	case <-testenv.WaitContext(t, ctx).Done():
		t.Fatal("work filing did not wake the waiting runner")
	}
	expect(call("new work after notification", preview(true, claim)), false, item.WorkItemID)

	claim.WorkItemID = item.WorkItemID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", token, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var held tracker.NativeLease
	decodeHubResponse(t, response, &held)
	claim.WorkItemID = ""
	claim.SessionID = "example-next"
	second := f.create(t, "queued behind active work")
	expect(call("active lease at capacity", preview(true, claim)), true)
	expect(call("legacy preview while at capacity", preview(false, claim)), false, second.WorkItemID)

	renewPath := f.base + "/leases/" + string(held.ID) + "/renew"
	response = performHubAPIRequest(t, f.service, http.MethodPost, renewPath, token, tracker.NativeLeaseMutation{FencingToken: held.FencingToken, TTLSeconds: 180})
	requireNativeStatus(t, response, http.StatusOK)
	var renewed tracker.NativeLease
	decodeHubResponse(t, response, &renewed)
	if renewed.ID != held.ID || renewed.FencingToken != held.FencingToken || !renewed.ExpiresAt.After(held.ExpiresAt) {
		t.Fatalf("renewal lost held lease or failed to extend it: %+v", renewed)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, renewPath, token, tracker.NativeLeaseMutation{FencingToken: held.FencingToken + 1, TTLSeconds: 180}), http.StatusConflict)

	repository := f.service.config.TenantRepository
	fence := leasestore.Fence{LeaseID: string(held.ID), FencingToken: int64(held.FencingToken)}
	attemptID := hubapi.NewID("attempt")
	now := f.service.config.Clock()
	if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attemptID, RunID: hubapi.NewID("run"), Fence: fence, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: "example/attempt"})
		if err != nil {
			return err
		}
		_, err = w.Release(t.Context(), fence, "released")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		state, err := r.(runnerstore.ClaimStateReader).RunnerClaimState(t.Context(), runner.binding.RunnerID, f.project.ID, "", 10, now)
		if err != nil {
			return err
		}
		if len(state.Unclassified) != 1 || state.Unclassified[0].AttemptID != attemptID {
			return fmt.Errorf("released attempt did not enter cleanup proof lookup")
		}
		if _, err := r.LatestRunEvent(t.Context(), f.project.ID, attemptID); !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("expected missing cleanup event, got %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	claim.LabelInclude = []string{"example-missing"}
	for range 2 {
		expect(call("idle with missing cleanup proof", preview(true, claim)), false)
	}
}
