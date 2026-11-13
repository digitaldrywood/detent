package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatContentAccounting(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ limited, selectedPolicy bool }{{}, {limited: true}, {limited: true, selectedPolicy: true}} {
		name := fmt.Sprintf("limited=%t", test.limited)
		if test.selectedPolicy {
			name += "/selected_policy"
		}
		t.Run(name, func(t *testing.T) {
			limited := test.limited
			objects := &changeObjectFixture{}
			details := &heartbeatBudgetDetails{DetailStore: objects}
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			f, repository := heartbeatWriterBudgetFixture(t, Config{Content: objects.call, RunDetails: details, Clock: func() time.Time { return now }})
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now,
				Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners"}}
			for i := range 4 {
				item := f.create(t, fmt.Sprintf("Accounting admission %d", i))
				view.Diagnostics.Admissions = append(view.Diagnostics.Admissions, runnerauth.DiagnosticAdmission{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: fmt.Sprintf("project_capacity_full_%d", i)})
			}
			if test.selectedPolicy {
				initial := view
				initial.Authority, initial.Diagnostics = "hub_configuration", nil
				path := f.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "project_configuration": initial}), http.StatusOK)
				selected := hubTestPolicy()
				selected.Configuration = &policy.Configuration{Prompt: "A distinct selected workflow", Behavior: json.RawMessage(`{"enabled":true}`)}
				selected = selected.WithID()
				view.SelectedPolicy = &selected
			}
			f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID)}
			f.service.database.hostedOrganization = f.project.OrganizationID
			limits := map[string]int64{"registered_runners": 100, "connected_runners": 100}
			before := heartbeatBudgetBytes(t, f)
			if limited {
				limits["collaboration_bytes"] = before + 1
			}
			hostedTestPlans(t, f.service, limits)
			var hostname string
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				machine, err := r.Machine(t.Context(), string(runner.binding.MachineID))
				hostname = machine.Hostname
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var mutationErr error
			var mutationMu sync.Mutex
			details.setAfterStore(func(ctx context.Context, _ attachment.ArchiveReference) {
				if ctx.Value(heartbeatBudgetContentKey{}) != true {
					return
				}
				err := repository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					machine, err := w.Machine(ctx, string(runner.binding.MachineID))
					if err != nil {
						return err
					}
					machine.Hostname = "changed-runner"
					return w.RegisterMachine(ctx, machine)
				})
				mutationMu.Lock()
				mutationErr = errors.Join(mutationErr, err)
				mutationMu.Unlock()
			})
			path := f.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
			request := newHubAPIRequest(t, http.MethodPost, path, runner.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "sprite_name": hostname, "project_configuration": view})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), heartbeatBudgetContentKey{}, true)))
			mutationMu.Lock()
			injectedErr := mutationErr
			mutationMu.Unlock()
			if injectedErr != nil {
				t.Fatal(injectedErr)
			}
			uploaded := details.uploads()
			if limited {
				if len(uploaded) != 0 {
					t.Errorf("consumption refusal uploaded %d objects", len(uploaded))
				}
				requireNativeStatus(t, response, http.StatusTooManyRequests)
				if used := heartbeatBudgetBytes(t, f); used != before {
					t.Fatalf("refused preparation charged bytes=%d want=%d", used, before)
				}
				return
			}
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if !strings.Contains(response.Body.String(), "Sprite name must match the runner hostname") {
				t.Fatalf("unexpected preparation refusal: %s", response.Body.String())
			}
			configuration := uploaded[:0]
			decisions := 0
			for _, ref := range uploaded {
				raw, err := details.ReadDetail(t.Context(), string(f.project.OrganizationID), ref)
				if err != nil {
					t.Fatal(err)
				}
				var event tracker.CollaborationEvent
				if json.Unmarshal(raw, &event) == nil && event.Type == "scheduler.decision" && event.Data.Decision != nil && event.Data.Decision.Source == "native_runner_admission" {
					decisions++
					if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						_, err := r.ArchiveObjectByReference(t.Context(), ref)
						return err
					}); !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("refused heartbeat registered admission decision payload: %v", err)
					}
					continue
				}
				configuration = append(configuration, ref)
			}
			if len(configuration) != 2 || decisions != len(view.Diagnostics.Admissions) {
				t.Fatalf("configuration objects=%d decision payloads=%d, want canonical configuration and reported observation plus one prepared decision per skipped admission (%d)", len(configuration), decisions, len(view.Diagnostics.Admissions))
			}
			uploaded = configuration
			want := before
			for _, ref := range uploaded {
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					object, err := r.ArchiveObjectByReference(t.Context(), ref)
					if err == nil && (object.Reference != ref || object.CollaborationBytes != ref.CompressedSize) {
						return fmt.Errorf("uploaded object registered bytes=%d want=%d", object.CollaborationBytes, ref.CompressedSize)
					}
					return err
				}); err != nil {
					t.Error(err)
				}
				want += ref.CompressedSize
			}
			if used := heartbeatBudgetBytes(t, f); used != want {
				t.Fatalf("charged bytes=%d want=%d", used, want)
			}
			if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				_, _, err := r.RunnerProjectObservation(t.Context(), runner.binding.RunnerID, f.project.ID, "")
				if err == nil {
					return errors.New("refused heartbeat persisted diagnostics")
				}
				if !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHeartbeatAndRenewWriterBudget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		admission  bool
		renew      bool
		hosted     bool
		concurrent bool
		statements int
	}{
		{name: "heartbeat", statements: 12},
		{name: "heartbeat admission", admission: true, statements: 40},
		{name: "heartbeat hosted admission", admission: true, hosted: true, statements: 60},
		{name: "heartbeat concurrent admission", admission: true, concurrent: true, statements: 40},
		{name: "renew", renew: true, statements: 12},
		{name: "hosted renew", renew: true, hosted: true, statements: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
			clock := &leaseTestClock{value: now.Add(-2 * time.Second)}
			details := &heartbeatBudgetDetails{DetailStore: &memoryRunDetails{}}
			f, repository := heartbeatWriterBudgetFixture(t, Config{RunDetails: details, Clock: clock.Now})
			runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			runner.enroll(t)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			item := f.create(t, "Writer budget")
			path := f.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
			body := map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation}
			warm := map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 2, ClientLimit: 2, RuntimeLimit: 2, Manageable: true, ObservedAt: clock.Now()}}
			if test.admission {
				view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now, Diagnostics: &runnerauth.ProjectDiagnostics{ObservedAt: now, Source: "runner_runtime_and_durable_attempt_owners", Admissions: []runnerauth.DiagnosticAdmission{{IssueID: string(item.WorkItemID), WorkItemRevision: int64(item.Revision), ObservedAt: now, Result: "skipped", Predicate: "project_capacity_full"}}}}
				body["project_configuration"] = view
				initial := view
				initial.Authority, initial.ObservedAt, initial.Diagnostics = "hub_configuration", clock.Now(), nil
				warm["project_configuration"] = initial
			}
			positive := newHeartbeatBudgetProbe()
			warmRequest := newHubAPIRequest(t, http.MethodPost, path, runner.redemption.Credential, warm)
			warmResponse := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(warmResponse, warmRequest.WithContext(context.WithValue(warmRequest.Context(), heartbeatBudgetProbeKey{}, positive)))
			requireNativeStatus(t, warmResponse, http.StatusOK)
			if txs, statements, _ := positive.snapshot(); txs == 0 || statements == 0 {
				t.Fatal("missing actual PostgreSQL heartbeat writer trace")
			}
			clock.Advance(2 * time.Second)
			var requestBody any = body
			if test.renew {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", runner.redemption.Credential, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: item.WorkItemID, MachineID: runner.binding.MachineID, SessionID: "writer-budget", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				path = f.base + "/leases/" + string(lease.ID) + "/renew"
				requestBody = tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}
			}
			f.service.tokenUseWork.Wait()
			if test.hosted {
				f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID)}
				f.service.database.hostedOrganization = f.project.OrganizationID
				hostedTestPlans(t, f.service, nil)
				metrics, err := newPostgresTestMetrics(t, f.project.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				f.service.requestMetrics = metrics
				t.Cleanup(func() {
					if err := f.service.Close(); err != nil {
						t.Error(err)
					}
				})
				f.service.echo.Use(f.service.hostedBoundary)
			}
			var injected atomic.Bool
			var innerResponse *httptest.ResponseRecorder
			if test.concurrent {
				f.service.config.TenantRepository = &heartbeatBudgetRepository{Repository: repository, beforeHeartbeat: func() {
					injected.Store(true)
					view := body["project_configuration"].(runnerauth.ProjectConfiguration)
					diagnostics := *view.Diagnostics
					diagnostics.Admissions = append([]runnerauth.DiagnosticAdmission{}, diagnostics.Admissions...)
					diagnostics.ObservedAt, diagnostics.Admissions[0].ObservedAt = now.Add(-time.Second), now.Add(-time.Second)
					diagnostics.Admissions[0].Predicate = "older_project_capacity_full"
					view.Diagnostics, view.ObservedAt = &diagnostics, now.Add(-time.Second)
					innerResponse = performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "project_configuration": view})
					clock.Advance(time.Second)
				}}
			}
			probe := newHeartbeatBudgetProbe()
			request := newHubAPIRequest(t, http.MethodPost, path, runner.redemption.Credential, requestBody)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), heartbeatBudgetProbeKey{}, probe)))
			if test.concurrent {
				if !injected.Load() {
					t.Fatal("concurrent admission was not injected")
				}
				requireNativeStatus(t, innerResponse, http.StatusOK)
			}
			requireNativeStatus(t, response, http.StatusOK)
			transactions, statements, queries := probe.snapshot()
			t.Logf("writer_txs=%d writer_statements=%d", transactions, statements)
			wantTransactions := int64(1)
			if test.admission {
				wantTransactions++
			}
			if queries == 0 || transactions != wantTransactions || statements > test.statements {
				t.Fatalf("writer_txs=%d (want %d), writer_statements=%d (bound %d), queries=%d", transactions, wantTransactions, statements, test.statements, queries)
			}
			if details.writesInWriter.Load() != 0 {
				t.Fatalf("object operations inside writer=%d", details.writesInWriter.Load())
			}
			if test.admission {
				assertHeartbeatBudgetAdmission(t, repository, f, runner.binding.RunnerID, item, now)
				if test.concurrent {
					clock.Advance(time.Second)
					older := body["project_configuration"].(runnerauth.ProjectConfiguration)
					diagnostics := *older.Diagnostics
					diagnostics.Admissions = append([]runnerauth.DiagnosticAdmission{}, diagnostics.Admissions...)
					diagnostics.ObservedAt, diagnostics.Admissions[0].ObservedAt = now.Add(-time.Second), now.Add(-time.Second)
					diagnostics.Admissions[0].Predicate = "older_project_capacity_full"
					older.Diagnostics, older.ObservedAt = &diagnostics, now.Add(-time.Second)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.redemption.Credential, map[string]any{"display_name": "runner", "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "project_configuration": older}), http.StatusOK)
					assertHeartbeatBudgetAdmission(t, repository, f, runner.binding.RunnerID, item, now)
					credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(runner.redemption.Credential), "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("admission authority status=%d: %v", status, err)
					}
					var observation runnerstore.ProjectObservation
					if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						observation, _, err = r.RunnerProjectObservation(t.Context(), runner.binding.RunnerID, f.project.ID, "")
						return err
					}); err != nil {
						t.Fatal(err)
					}
					conflict := body["project_configuration"].(runnerauth.ProjectConfiguration)
					conflicting := *conflict.Diagnostics
					conflicting.Admissions = append([]runnerauth.DiagnosticAdmission{}, conflicting.Admissions...)
					conflicting.Admissions[0].Predicate = "conflicting_same_time"
					conflict.Diagnostics = &conflicting
					err = repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
						if err := f.service.authorizeAccessScope(t.Context(), w, scope, true); err != nil {
							return err
						}
						return storeRunnerProjectObservationTenant(t.Context(), scope, conflict, observation.ObservedAt, w)
					})
					if !errors.Is(err, runnerstore.ErrStaleObservation) {
						t.Fatalf("same-time conflicting snapshot error=%v", err)
					}
					assertHeartbeatBudgetAdmission(t, repository, f, runner.binding.RunnerID, item, now)
				}
			}
		})
	}
}

func heartbeatWriterBudgetFixture(t *testing.T, config Config) (nativeFixture, *postgres.Repository) {
	t.Helper()
	repository, _ := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, heartbeatBudgetTrace{})
	})
	config.TenantRepository = repository
	f := newDefaultNativeFixture(t, config)
	if err := f.service.database.postgres.Close(); err != nil {
		t.Fatal(err)
	}
	f.service.database.postgres, f.service.requestMetrics.Repository = repository, repository
	f.service.config.TenantRepository, f.service.config.AccessRepository, f.service.config.BillingRepository = repository, repository, repository
	f.service.database.billingRepository = repository
	return f, repository
}

func heartbeatBudgetBytes(t *testing.T, f nativeFixture) int64 {
	t.Helper()
	var used int64
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		used, err = r.CollaborationBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return used
}

func assertHeartbeatBudgetAdmission(t *testing.T, repository tenantstore.Repository, f nativeFixture, runnerID string, item tracker.NativeIssue, at time.Time) {
	t.Helper()
	if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		observation, _, err := r.RunnerProjectObservation(t.Context(), runnerID, f.project.ID, string(item.WorkItemID))
		if err != nil {
			return err
		}
		if observation.RunnerID != runnerID || observation.Diagnostics == nil || len(observation.Diagnostics.Admissions) != 1 {
			return fmt.Errorf("admission observation lost owner/cardinality: %+v", observation)
		}
		admission := observation.Diagnostics.Admissions[0]
		if admission.IssueID != string(item.WorkItemID) || admission.WorkItemRevision != int64(item.Revision) || admission.Result != "skipped" || admission.Predicate != "project_capacity_full" || !admission.ObservedAt.Equal(at) {
			return fmt.Errorf("latest admission=%+v, want current outer observation at %s", admission, at)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type heartbeatBudgetRepository struct {
	*postgres.Repository
	beforeHeartbeat func()
	injected        atomic.Bool
}

func (r *heartbeatBudgetRepository) WriteRunnerHeartbeat(ctx context.Context, org tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	if r.injected.CompareAndSwap(false, true) {
		r.beforeHeartbeat()
	}
	return r.Repository.WriteRunnerHeartbeat(ctx, org, credential, runner, write)
}

type heartbeatBudgetContentKey struct{}
type heartbeatBudgetDetails struct {
	attachment.DetailStore
	mu             sync.Mutex
	afterStore     func(context.Context, attachment.ArchiveReference)
	uploaded       map[string]attachment.ArchiveReference
	writesInWriter atomic.Int64
}

func (s *heartbeatBudgetDetails) setAfterStore(hook func(context.Context, attachment.ArchiveReference)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterStore = hook
}
func (s *heartbeatBudgetDetails) uploads() []attachment.ArchiveReference {
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := make([]attachment.ArchiveReference, 0, len(s.uploaded))
	for _, ref := range s.uploaded {
		refs = append(refs, ref)
	}
	return refs
}
func (s *heartbeatBudgetDetails) checkWriter(ctx context.Context) {
	if p, ok := ctx.Value(heartbeatBudgetProbeKey{}).(*heartbeatBudgetProbe); ok && p.active() {
		s.writesInWriter.Add(1)
	}
}
func (s *heartbeatBudgetDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	s.checkWriter(ctx)
	ref, err := s.DetailStore.StoreDetail(ctx, organization, raw)
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	if ctx.Value(heartbeatBudgetContentKey{}) == true {
		if s.uploaded == nil {
			s.uploaded = make(map[string]attachment.ArchiveReference)
		}
		s.uploaded[ref.Key] = ref
	}
	hook := s.afterStore
	s.mu.Unlock()
	if hook != nil {
		hook(ctx, ref)
	}
	return ref, nil
}
func (s *heartbeatBudgetDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	s.checkWriter(ctx)
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

type heartbeatBudgetProbeKey struct{}
type heartbeatBudgetProbe struct {
	mu           sync.Mutex
	writers      map[*pgx.Conn]bool
	transactions int64
	statements   int
	queries      int
}

func newHeartbeatBudgetProbe() *heartbeatBudgetProbe {
	return &heartbeatBudgetProbe{writers: make(map[*pgx.Conn]bool)}
}
func (p *heartbeatBudgetProbe) active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writers) > 0
}
func (p *heartbeatBudgetProbe) snapshot() (int64, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.transactions, p.statements, p.queries
}
func traceHeartbeatBudget(ctx context.Context, conn *pgx.Conn, query string) {
	p, ok := ctx.Value(heartbeatBudgetProbeKey{}).(*heartbeatBudgetProbe)
	if !ok {
		return
	}
	statement := strings.ToLower(strings.TrimSpace(query))
	for strings.HasPrefix(statement, "--") {
		_, rest, found := strings.Cut(statement, "\n")
		if !found {
			return
		}
		statement = strings.TrimSpace(rest)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries++
	if strings.HasPrefix(statement, "begin") {
		if !strings.Contains(statement, "read only") {
			p.transactions++
			p.writers[conn] = true
		}
		return
	}
	if p.writers[conn] && statement != "commit" && statement != "rollback" {
		p.statements++
	}
}
func endHeartbeatBudget(ctx context.Context, conn *pgx.Conn) {
	if p, ok := ctx.Value(heartbeatBudgetProbeKey{}).(*heartbeatBudgetProbe); ok && conn.PgConn().TxStatus() == 'I' {
		p.mu.Lock()
		delete(p.writers, conn)
		p.mu.Unlock()
	}
}

type heartbeatBudgetTrace struct{}

func (heartbeatBudgetTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	traceHeartbeatBudget(ctx, conn, data.SQL)
	return ctx
}
func (heartbeatBudgetTrace) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceQueryEndData) {
	endHeartbeatBudget(ctx, conn)
}
func (heartbeatBudgetTrace) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		traceHeartbeatBudget(ctx, conn, query.SQL)
	}
	return ctx
}
func (heartbeatBudgetTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}
func (heartbeatBudgetTrace) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	endHeartbeatBudget(ctx, conn)
}
