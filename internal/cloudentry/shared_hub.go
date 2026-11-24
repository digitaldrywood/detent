package cloudentry

import (
	"container/list"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudentry/internal/keylock"
	"github.com/digitaldrywood/detent/internal/cloudentry/platformstore"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SharedHubConfig struct {
	runMaintenance func(*hubmaintenance.Scheduler, context.Context, func(error)) error
	Postgres       postgres.Config
	Configure      func(TenantSpec) ([]byte, error)
	TenantConfig   func([]byte, TenantSpec) (hubserver.Config, error)
}

type sharedHub struct {
	idleLimit     int
	opening       int
	idle          list.List
	closing       map[string]chan struct{}
	entry         *Service
	runtime       *hubserver.HostedRuntime
	transport     *http.Transport
	server        *http.Server
	listener      *hubListener
	done          chan struct{}
	lifecycle     keylock.Mutex
	mu            sync.Mutex
	tenants       map[string]*servedTenant
	closed        bool
	starting      sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	scheduler     *hubmaintenance.Scheduler
	schedulerWork sync.WaitGroup
}

type servedTenant struct {
	id         string
	idle       *list.Element
	users      int
	service    *hubserver.Service
	provider   string
	generation int64
	ctx        context.Context
	cancel     context.CancelFunc
	work       sync.WaitGroup
	features   map[string]hubmaintenance.Feature
}

func openSharedHub(ctx context.Context, entry *Service, config *SharedHubConfig) (*sharedHub, error) {
	runtime, err := hubserver.OpenHostedRuntime(ctx, config.Postgres)
	if err != nil {
		return nil, err
	}
	h := &sharedHub{idleLimit: hubmaintenance.TenantLimit, entry: entry, runtime: runtime, tenants: make(map[string]*servedTenant), closing: make(map[string]chan struct{}), listener: &hubListener{incoming: make(chan net.Conn), done: make(chan struct{})}, done: make(chan struct{})}
	h.ctx, h.cancel = context.WithCancel(context.WithoutCancel(ctx))
	h.server = &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	h.transport = &http.Transport{DialContext: h.listener.dial, DisableCompression: true, IdleConnTimeout: time.Minute, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: 16}
	go func() {
		defer close(h.done)
		if err := h.server.Serve(h.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			entry.config.Logger.Error("serve shared Hub", "error", err)
		}
	}()
	return h, nil
}

func (h *sharedHub) Start(ctx context.Context, spec TenantSpec) error {
	return h.ensureTenant(ctx, spec.Organization)
}

func (h *sharedHub) ensureTenant(ctx context.Context, org Organization) error {
	t, err := h.acquireTenant(ctx, org)
	if err == nil {
		h.release(ctx, t)
	}
	return err
}

func (h *sharedHub) pin(t *servedTenant) {
	if t.idle != nil {
		h.idle.Remove(t.idle)
		t.idle = nil
	}
	t.users++
	t.work.Add(1)
}

func (h *sharedHub) acquireTenant(ctx context.Context, org Organization) (*servedTenant, error) {
	id := org.ID
	h.mu.Lock()
	t := h.tenants[id]
	if !h.closed && t != nil && t.generation == org.Generation && t.provider == org.ProviderID {
		h.pin(t)
		h.mu.Unlock()
		return t, nil
	}
	h.mu.Unlock()
	if t != nil && org.Generation <= t.generation {
		return nil, ErrOrganizationConflict
	}
	ctx, cancelStartup := context.WithTimeout(ctx, h.entry.config.tenantStartTimeout)
	defer cancelStartup()
	unlock, err := h.lifecycle.LockContext(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	h.mu.Lock()
	for done := h.closing[id]; done != nil; done = h.closing[id] {
		h.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		h.mu.Lock()
	}
	if h.closed {
		h.mu.Unlock()
		return nil, net.ErrClosed
	}
	existing := h.tenants[id]
	if existing != nil && org.Generation <= existing.generation {
		defer h.mu.Unlock()
		if org.Generation != existing.generation || org.ProviderID != existing.provider {
			return nil, ErrOrganizationConflict
		}
		h.pin(existing)
		return existing, nil
	}
	if existing == nil && len(h.tenants)+len(h.closing)+h.opening >= h.idleLimit+hubmaintenance.TenantLimit {
		h.mu.Unlock()
		return nil, corestore.ErrServingCapacity
	}
	h.opening++
	h.starting.Add(1)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.opening--
		h.mu.Unlock()
		h.starting.Done()
	}()
	current, err := h.entry.registry.Organization(ctx, id)
	if err != nil {
		return nil, err
	}
	if !servesGeneration(current, org.ProviderID, org.Generation) {
		return nil, ErrOrganizationConflict
	}
	if existing != nil {
		if err := h.stopTenant(id); err != nil {
			return nil, err
		}
	}
	cfg, err := h.tenantConfig(ctx, current)
	if err != nil {
		return nil, err
	}
	organizationID := tracker.OrganizationID(id)
	if current.Managed && current.State == "allocating" && current.Step == "tenant_files" {
		if err := h.runtime.ProvisionTenant(ctx, organizationID, current.Name, current.Slug, cfg); err != nil {
			return nil, err
		}
	}
	service, err := h.runtime.OpenTenant(ctx, organizationID, cfg)
	if err != nil {
		return nil, err
	}
	seconds, err := service.BillingReconcileSeconds(ctx)
	if err == nil {
		err = cfg.RecordBillingSchedule(ctx, seconds)
	}
	if err != nil {
		return nil, errors.Join(err, service.Close())
	}
	lifetime, cancel := context.WithCancel(context.Background())
	features := service.SharedMaintenanceFeatures()
	tenant := &servedTenant{id: id, service: service, provider: current.ProviderID, generation: current.Generation, ctx: lifetime, cancel: cancel, features: make(map[string]hubmaintenance.Feature, len(features))}
	for _, feature := range features {
		tenant.features[feature.Name] = feature
	}
	h.mu.Lock()
	h.pin(tenant)
	h.tenants[id] = tenant
	h.mu.Unlock()

	return tenant, nil
}

func (h *sharedHub) tenantConfig(ctx context.Context, current Organization) (hubserver.Config, error) {
	spec := h.entry.tenantSpec(current)
	config := h.entry.config.SharedHub
	configure := config.Configure
	state, err := h.entry.loadRuntime(ctx)
	if err != nil {
		return hubserver.Config{}, err
	}
	if state != nil && state.Configure != nil {
		configure = state.Configure
	}
	cfg, err := h.entry.hostedTenantConfig(ctx, spec, configure)
	if err != nil {
		return hubserver.Config{}, err
	}
	cfg.Version, cfg.Commit = h.entry.config.Build.Version, h.entry.config.Build.Commit

	cfg.ClaimBillingReconciliation = func(ctx context.Context) (bool, error) {
		return h.entry.registry.store.db.postgres.BeginBillingReconciliation(ctx, hubmaintenance.Tenant{Organization: current.ID, Generation: current.Generation}, h.entry.config.Now())
	}
	cfg.RecordBillingSchedule = func(ctx context.Context, seconds int64) error {
		return h.entry.registry.store.db.postgres.ConfigureBillingSchedule(ctx, hubmaintenance.Tenant{Organization: current.ID, Generation: current.Generation}, seconds, h.entry.config.Now())
	}
	return cfg, nil
}
func (h *sharedHub) Page(ctx context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	return h.entry.RuntimeTenantPage(ctx, after, limit)
}

func (h *sharedHub) startScheduler() error {
	features := hubserver.HostedMaintenanceSchedule(h.step)
	features = append([]hubmaintenance.Feature{h.entry.gitHubFanoutFeature()}, features...)
	for i := range features {
		if features[i].Name == "billing-reconciliation" {
			var discovery platformstore.BillingScheduleDiscovery
			features[i].Due = func(ctx context.Context, limit int) ([]hubmaintenance.Tenant, error) {
				return discovery.Page(ctx, h.entry.registry.store.db.postgres, h.entry.config.Now(), limit)
			}
		}
	}
	features = append(features, hubmaintenance.Feature{Name: "runtime-adoption", Bytes: hubmaintenance.ByteLimit / hubmaintenance.TenantLimit, Step: func(ctx context.Context, t hubmaintenance.Tenant, b hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		return h.step(ctx, t, b, "runtime-adoption")
	}})
	scheduler, err := hubmaintenance.NewShared(h, features)
	if err != nil {
		return err
	}
	h.scheduler = scheduler
	run := h.entry.config.SharedHub.runMaintenance
	if run == nil {
		run = (*hubmaintenance.Scheduler).Run
	}
	h.schedulerWork.Go(func() {
		if err := run(scheduler, h.ctx, func(err error) { h.entry.config.Logger.Warn("maintain hosted organizations", "error", err) }); err != nil && !errors.Is(err, context.Canceled) {
			h.entry.config.Logger.Error("stop shared Hub scheduler", "error", err)
		}
	})
	return nil
}

func (h *sharedHub) step(ctx context.Context, selected hubmaintenance.Tenant, budget hubmaintenance.Budget, name string) (outcome hubmaintenance.Outcome, err error) {
	if err := hubmaintenance.RequireTenant(ctx, selected); err != nil {
		return outcome, err
	}
	ctx, cancel := context.WithTimeout(entryBackground(ctx), 30*time.Second)
	defer cancel()
	org, err := h.entry.registry.Organization(ctx, selected.Organization)
	if err != nil || org.State != "ready" || org.Generation != selected.Generation {
		return outcome, err
	}
	if name == "secret-rewrap" {
		unlock, err := h.lifecycle.LockContext(ctx, org.ID)
		if err != nil {
			return outcome, err
		}
		defer unlock()
		current, err := h.entry.registry.Organization(ctx, org.ID)
		if err != nil {
			return outcome, err
		}
		if current.State != "ready" || current.Generation != selected.Generation || current.ProviderID != org.ProviderID {
			return outcome, ErrOrganizationConflict
		}
		cfg, err := h.tenantConfig(ctx, current)
		if err != nil {
			return outcome, err
		}
		return h.runtime.MaintainSecret(ctx, tracker.OrganizationID(org.ID), selected.Generation, cfg)
	}
	if name == "runtime-adoption" {
		err := h.entry.AdoptPublishedRuntime(ctx, selected)
		if errors.Is(err, corestore.ErrNotFound) {
			return outcome, nil
		}
		return hubmaintenance.Outcome{Items: 1}, err
	}
	tenant, err := h.acquireTenant(ctx, org)
	if err != nil {
		return outcome, err
	}
	defer h.release(ctx, tenant)
	stop := context.AfterFunc(tenant.ctx, cancel)
	defer stop()
	feature, ok := tenant.features[name]
	if !ok {
		return outcome, nil
	}
	return feature.Step(ctx, selected, budget)
}

func (h *sharedHub) AdoptRuntime(ctx context.Context, selected Organization, expected string, update hostedplans.HostedRuntimeUpdate) error {
	org, err := h.entry.registry.Organization(ctx, selected.ID)
	if err != nil {
		return err
	}
	if org.Generation != selected.Generation || !org.Managed || org.State != "allocating" && org.State != "ready" {
		return ErrOrganizationConflict
	}
	t, err := h.acquireTenant(ctx, org)
	if err != nil {
		return err
	}
	defer h.release(ctx, t)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	defer func() { stop(); cancel() }()
	return t.service.AdoptRuntime(ctx, expected, update)
}

func (h *sharedHub) unpin(t *servedTenant, cleanOnly bool) []*servedTenant {
	h.mu.Lock()
	defer h.mu.Unlock()
	t.users--
	t.work.Done()
	if t.users != 0 || h.closed || h.tenants[t.id] != t {
		return nil
	}
	t.idle = h.idle.PushBack(t)
	victims := make([]*servedTenant, 0, 2)
	for range 2 {
		victim := h.evictable(t, cleanOnly)
		if victim == nil {
			break
		}
		victims = append(victims, victim)
		h.idle.Remove(victim.idle)
		victim.idle = nil
		delete(h.tenants, victim.id)
		h.closing[victim.id] = make(chan struct{})
		h.starting.Add(1)
	}
	return victims
}

func (h *sharedHub) release(ctx context.Context, t *servedTenant) {
	job := hubmaintenance.RequireTenant(ctx, hubmaintenance.Tenant{Organization: t.id, Generation: t.generation}) == nil
	for _, victim := range h.unpin(t, job) {
		parent := h.ctx
		if job {
			parent = ctx
		}
		cleanup, cancel := context.WithTimeout(postgres.WithBackgroundWork(parent), 30*time.Second)
		ready := !victim.service.PendingRequestMetrics()
		var err error
		if !ready && !job {
			ready, err = victim.service.PrepareIdleClose(cleanup)
		}
		if ready {
			err = closeServedTenantContext(cleanup, victim)
		}
		cancel()
		h.mu.Lock()
		if !ready {
			h.tenants[victim.id] = victim
			victim.idle = h.idle.PushBack(victim)
		}
		close(h.closing[victim.id])
		delete(h.closing, victim.id)
		h.mu.Unlock()
		h.starting.Done()
		if err != nil {
			h.entry.config.Logger.Error("evict idle hosted service", "organization", victim.id, "error", err)
		}
	}
}

func (h *sharedHub) evictable(released *servedTenant, cleanOnly bool) *servedTenant {
	if h.idle.Len() <= h.idleLimit {
		return nil
	}
	for range min(h.idle.Len(), hubmaintenance.TenantLimit) {
		head := h.idle.Front()
		t := head.Value.(*servedTenant)
		if !t.service.HostedActivity() && (!cleanOnly || !t.service.PendingRequestMetrics()) {
			return t
		}
		h.idle.MoveToBack(head)
	}
	if released.idle != nil && !released.service.HostedActivity() && (!cleanOnly || !released.service.PendingRequestMetrics()) {
		return released
	}
	return nil
}

func (h *sharedHub) WaitReady(ctx context.Context, id string) error {
	org, err := h.entry.registry.Organization(ctx, id)
	if err != nil {
		return err
	}
	return h.ensureTenant(ctx, org)
}

func (*sharedHub) Failure(string) error { return nil }

func (h *sharedHub) Stop(id string) error {
	unlock := h.lifecycle.Lock(id)
	defer unlock()
	return h.stopTenant(id)
}

func (h *sharedHub) stopTenant(id string) error {
	h.mu.Lock()
	for done := h.closing[id]; done != nil; done = h.closing[id] {
		h.mu.Unlock()
		<-done
		h.mu.Lock()
	}
	t := h.tenants[id]
	if t != nil && t.idle != nil {
		h.idle.Remove(t.idle)
		t.idle = nil
	}
	delete(h.tenants, id)
	h.mu.Unlock()
	if t == nil {
		return nil
	}
	return closeServedTenant(t)
}

func closeServedTenant(t *servedTenant) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return closeServedTenantContext(ctx, t)
}

func closeServedTenantContext(ctx context.Context, t *servedTenant) error {
	t.cancel()
	err := t.service.Shutdown(ctx)
	t.work.Wait()
	return errors.Join(err, t.service.CloseContext(ctx))
}

func (h *sharedHub) Close() error {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.cancel()
	h.starting.Wait()
	h.schedulerWork.Wait()
	h.transport.CloseIdleConnections()
	err := h.server.Close()
	<-h.done
	h.mu.Lock()
	ids := make([]string, 0, len(h.tenants))
	for id := range h.tenants {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		err = errors.Join(err, h.Stop(id))
	}
	return errors.Join(err, h.runtime.Close())
}

func servesGeneration(current Organization, provider string, generation int64) bool {
	return current.ProviderID == provider && current.Generation == generation && (current.State == "ready" || current.Managed && current.State == "allocating")
}

func (h *sharedHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, generation, ok := strings.Cut(r.Host, ".")
	h.mu.Lock()
	t := h.tenants[id]
	if !ok || h.closed || t == nil || generation != strconv.FormatInt(t.generation, 10) {
		h.mu.Unlock()
		http.Error(w, "tenant unavailable", http.StatusServiceUnavailable)
		return
	}
	h.pin(t)
	h.mu.Unlock()
	defer h.release(r.Context(), t)
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	defer func() { stop(); cancel() }()
	current, err := h.entry.registry.Organization(ctx, id)
	if err != nil || !servesGeneration(current, t.provider, t.generation) {
		http.Error(w, "tenant unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Host = "tenant"
	t.service.Handler().ServeHTTP(w, r.WithContext(ctx))
}

type hubRoute struct {
	host         *sharedHub
	organization Organization
}

func (route hubRoute) RoundTrip(r *http.Request) (*http.Response, error) {
	h := route.host
	t, err := h.acquireTenant(r.Context(), route.organization)
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	defer h.release(r.Context(), t)
	request := r.Clone(r.Context())
	request.Host = route.organization.ID + "." + strconv.FormatInt(route.organization.Generation, 10)
	return h.transport.RoundTrip(request)
}

type hubListener struct {
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func (l *hubListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.incoming <- server:
		return client, nil
	case <-l.done:
	case <-ctx.Done():
	}
	return nil, errors.Join(net.ErrClosed, ctx.Err(), client.Close(), server.Close())
}
func (l *hubListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.incoming:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *hubListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*hubListener) Addr() net.Addr { return &net.UnixAddr{Name: "shared-hub", Net: "pipe"} }
