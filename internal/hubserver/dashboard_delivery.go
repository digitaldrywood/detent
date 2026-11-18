package hubserver

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dashboardCredentialWriter struct{ header http.Header }

func (w *dashboardCredentialWriter) Header() http.Header         { return w.header }
func (w *dashboardCredentialWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *dashboardCredentialWriter) WriteHeader(int)             {}

func dashboardJitter(d time.Duration) time.Duration {
	return d - d/10 + time.Duration(float64(d/5)*hubdeps.RandomJitter())
}

type dashboardFrame struct {
	sequence int64
	event    string
	data     string
	terminal bool
}

type dashboardSubscription struct {
	request      *http.Request
	claims       any
	scope        hubscope.NativeScope
	params       url.Values
	workspace    string
	item         string
	board        bool
	initial      bool
	sequence     int64
	grantVersion string
	epoch        uint64
	access       viewstore.BoardDeliveryAccess
	authAt       time.Time
	more         bool
	terminal     bool
	frames       chan dashboardFrame
	done         chan struct{}
	abort        func()
	closed       atomic.Bool
	closeOnce    sync.Once
	element      *list.Element
}

func (s *dashboardSubscription) close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
		if s.abort != nil {
			s.abort()
		}
	})
}

func (s *dashboardSubscription) enqueue(frame dashboardFrame) bool {
	if s.closed.Load() {
		return false
	}
	select {
	case s.frames <- frame:
		return true
	default:
		s.close()
		return false
	}
}

func (s *dashboardSubscription) reset(sequence int64) {
	for {
		select {
		case <-s.frames:
		default:
			s.terminal = true
			s.enqueue(dashboardFrame{sequence: sequence, event: "reset", data: "{}", terminal: true})
			return
		}
	}
}

type dashboardProjectKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

func (s *dashboardSubscription) projectKey() dashboardProjectKey {
	return dashboardProjectKey{s.scope.Organization, s.scope.Project}
}

type dashboardProjectState struct {
	head   viewstore.BoardSequence
	live   bool
	access map[*dashboardSubscription]viewstore.BoardDeliveryAccess
	err    error
}

type dashboardDelivery struct {
	service   *Service
	config    hubdeps.DashboardDelivery
	mu        sync.Mutex
	subs      list.List
	tenants   map[tracker.OrganizationID]int
	projects  map[dashboardProjectKey]map[*dashboardSubscription]struct{}
	closed    bool
	wake      chan struct{}
	cancel    context.CancelFunc
	workers   sync.WaitGroup
	epoch     uint64
	remaining int
	states    map[dashboardProjectKey]dashboardProjectState
}

func newDashboardDelivery(s *Service) *dashboardDelivery {
	cfg := hubdeps.DashboardDelivery{NewTimer: hubdeps.NewTimer, PageSize: 64, QueueSize: 128, TenantLimit: 128, Budget: 32}
	if override := s.config.deps.DashboardDelivery; override != nil {
		if override.NewTimer != nil {
			cfg.NewTimer = override.NewTimer
		}
		if override.PageSize > 0 {
			cfg.PageSize = override.PageSize
		}
		if override.QueueSize > 0 {
			cfg.QueueSize = override.QueueSize
		}
		if override.TenantLimit > 0 {
			cfg.TenantLimit = override.TenantLimit
		}
		if override.Budget > 0 {
			cfg.Budget = override.Budget
		}
	}
	return &dashboardDelivery{
		service: s, config: cfg, tenants: make(map[tracker.OrganizationID]int),
		projects: make(map[dashboardProjectKey]map[*dashboardSubscription]struct{}),
		states:   make(map[dashboardProjectKey]dashboardProjectState), wake: make(chan struct{}, 1),
	}
}

func (d *dashboardDelivery) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.workers.Go(func() { d.run(ctx) })
}

func (d *dashboardDelivery) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	if d.cancel != nil {
		d.cancel()
	}
	d.mu.Unlock()
	d.workers.Wait()
}

func (d *dashboardDelivery) add(sub *dashboardSubscription) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.tenants[sub.scope.Organization] >= d.config.TenantLimit {
		return false
	}
	if d.service.config.runtimeHost != nil && d.cancel == nil {
		d.start()
	}
	sub.frames = make(chan dashboardFrame, d.config.QueueSize)
	sub.done = make(chan struct{})
	sub.more = true
	sub.element = d.subs.PushBack(sub)
	key := sub.projectKey()
	if d.projects[key] == nil {
		d.projects[key] = make(map[*dashboardSubscription]struct{})
	}
	d.projects[key][sub] = struct{}{}
	d.tenants[sub.scope.Organization]++
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return true
}

func (d *dashboardDelivery) remove(sub *dashboardSubscription) {
	sub.close()
	d.mu.Lock()
	defer d.mu.Unlock()
	if sub.element != nil {
		defer func() {
			select {
			case d.wake <- struct{}{}:
			default:
			}
		}()
		d.subs.Remove(sub.element)
		sub.element = nil
		key := sub.projectKey()
		delete(d.projects[key], sub)
		if len(d.projects[key]) == 0 {
			delete(d.projects, key)
		}
		d.tenants[sub.scope.Organization]--
		if d.tenants[sub.scope.Organization] == 0 {
			delete(d.tenants, sub.scope.Organization)
		}
	}
}

func (d *dashboardDelivery) changed() {
	d.epoch++
	d.states = make(map[dashboardProjectKey]dashboardProjectState)
	d.mu.Lock()
	d.remaining = d.subs.Len()
	d.mu.Unlock()
}

func (d *dashboardDelivery) projectState(ctx context.Context, key dashboardProjectKey, now time.Time) dashboardProjectState {
	if state, ok := d.states[key]; ok {
		return state
	}
	d.mu.Lock()
	members := d.projects[key]
	subs := make([]*dashboardSubscription, 0, len(members))
	subjects := make([]viewstore.BoardDeliverySubject, 0, len(members))
	for sub := range members {
		authority := d.service.accessAuthority(sub.scope, false)
		subs = append(subs, sub)
		subjects = append(subjects, viewstore.BoardDeliverySubject{
			CredentialID: authority.CredentialID, CredentialHash: authority.Hash, UserID: authority.UserID,
			MembershipID: authority.MembershipID, SessionHash: authority.SessionHash,
		})
	}
	d.mu.Unlock()
	if len(subjects) == 0 {
		return dashboardProjectState{}
	}
	state := dashboardProjectState{}
	repository := d.service.config.TenantRepository
	if repository == nil {
		state.err = hubapi.NativeWorkItemStorageUnavailable()
	} else {
		state.err = repository.ReadTenant(ctx, key.organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(viewstore.BoardDeliveryReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			current, err := reader.BoardDeliveryState(ctx, key.project, subjects, now)
			if err != nil {
				return err
			}
			if len(current.Access) != len(subs) {
				return viewstore.ErrInvalidView
			}
			state.head, state.live = current.Head, current.ProjectLive
			state.access = make(map[*dashboardSubscription]viewstore.BoardDeliveryAccess, len(subs))
			for i, sub := range subs {
				state.access[sub] = current.Access[i]
			}
			return nil
		})
	}
	d.states[key] = state
	return state
}

func (d *dashboardDelivery) run(ctx context.Context) {
	idle := false
	defer func() {
		if idle {
			return
		}
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
		for {
			d.mu.Lock()
			e := d.subs.Front()
			var sub *dashboardSubscription
			if e != nil {
				var ok bool
				sub, ok = e.Value.(*dashboardSubscription)
				if !ok {
					panic("invalid dashboard subscription")
				}
			}
			d.mu.Unlock()
			if sub == nil {
				return
			}
			d.remove(sub)
		}
	}()
	timer := d.config.NewTimer(dashboardJitter(time.Second))
	timer.Stop()
	var recoveryTicks <-chan time.Time
	defer timer.Stop()
	continuation := d.config.NewTimer(dashboardJitter(10 * time.Millisecond))
	continuation.Stop()
	var continuationTicks <-chan time.Time
	defer continuation.Stop()
	for {
		recoverTick := false
		select {
		case <-ctx.Done():
			return
		case <-d.service.database.dashboardCommitted:
			recoverTick = true
		case <-d.wake:
			d.mu.Lock()
			d.remaining = d.subs.Len()
			d.mu.Unlock()
		case <-continuationTicks:
		case <-recoveryTicks:
			recoverTick = true
			timer.Reset(dashboardJitter(time.Second))
		}
		d.mu.Lock()
		active := d.subs.Len() > 0
		if !active && !d.closed && d.service.config.runtimeHost != nil {
			d.cancel()
			d.cancel = nil
			idle = true
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()
		if !active {
			timer.Stop()
			continuation.Stop()
			recoveryTicks, continuationTicks = nil, nil
			continue
		}
		if recoveryTicks == nil {
			recoveryTicks = timer.C
			timer.Reset(dashboardJitter(time.Second))
			recoverTick = true
		}
		if recoverTick {
			d.changed()
		}

		d.deliver(ctx)
		if d.remaining > 0 {
			continuation.Reset(dashboardJitter(10 * time.Millisecond))
			continuationTicks = continuation.C
		} else {
			continuation.Stop()
			continuationTicks = nil
		}
	}
}

func (d *dashboardDelivery) deliver(ctx context.Context) {
	for range d.config.Budget {
		if d.remaining <= 0 || ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		e := d.subs.Front()
		if e == nil {
			d.remaining = 0
			d.mu.Unlock()
			return
		}
		sub, ok := e.Value.(*dashboardSubscription)
		if !ok {
			panic("invalid dashboard subscription")
		}
		d.subs.MoveToBack(e)
		d.remaining--
		d.mu.Unlock()
		if sub.closed.Load() {
			d.remove(sub)
			continue
		}
		if sub.terminal || !sub.more && sub.epoch == d.epoch {
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
		now := d.service.config.Clock()
		state := d.projectState(readCtx, sub.projectKey(), now)
		access, observed := state.access[sub]
		if state.err == nil && observed && !sub.more && !sub.initial && state.live &&
			sub.sequence == state.head.Sequence && sub.sequence >= state.head.ReplayFloor &&
			access.Live && access == sub.access && (access.ExpiresAt.IsZero() || now.Before(access.ExpiresAt)) &&
			now.Before(sub.authAt) {
			sub.epoch = d.epoch
			cancel()
			continue
		}
		more, err := false, state.err
		if err == nil {
			more, err = d.service.dashboardPage(readCtx, d, sub)
			if err == nil && observed {
				if !now.Before(sub.authAt) || access != sub.access {
					sub.authAt = now.Add(20 * time.Second)
				}
				sub.access = access
			}
		}
		cancel()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				d.service.config.Logger.ErrorContext(ctx, "deliver dashboard page", "error", err)
			}
			sub.reset(sub.sequence)
			more = false
		}
		sub.epoch, sub.more = d.epoch, more
		if more {
			d.mu.Lock()
			d.remaining = max(d.remaining, d.subs.Len())
			d.mu.Unlock()
		}
	}
}

func (s *Service) serveDashboardEvents(c echo.Context, scope hubscope.NativeScope, sequence int64, initial, board bool, workspace, item string) error {
	d := s.dashboard
	if d == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	sub := &dashboardSubscription{request: c.Request().Clone(c.Request().Context()), claims: c.Get(hostedSharedClaimsKey), scope: scope, params: c.QueryParams(), workspace: workspace, item: item, board: board, initial: initial, sequence: sequence}
	sub.params.Del("board")
	sub.params.Del("since")
	controller := http.NewResponseController(c.Response().Writer)
	setDeadline := func(at time.Time) bool {
		if err := controller.SetWriteDeadline(at); err != nil && !errors.Is(err, http.ErrNotSupported) {
			s.config.Logger.ErrorContext(c.Request().Context(), "set dashboard write deadline", "error", err)
			return false
		}
		return true
	}
	sub.abort = func() { setDeadline(time.Now()) }
	if !d.add(sub) {
		return c.NoContent(http.StatusTooManyRequests)
	}
	defer func() { d.remove(sub); setDeadline(time.Time{}) }()
	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("X-Accel-Buffering", "no")
	timer := d.config.NewTimer(dashboardJitter(20 * time.Second))
	defer timer.Stop()
	c.Response().WriteHeader(http.StatusOK)
	c.Response().Flush()
	for {
		select {
		case <-c.Request().Context().Done():
			return nil
		case <-sub.done:
			return nil
		case frame := <-sub.frames:
			if sub.closed.Load() {
				return nil
			}
			if !setDeadline(time.Now().Add(5 * time.Second)) {
				return nil
			}
			if _, err := fmt.Fprintf(c.Response(), "id: %d\nevent: %s\ndata: %s\n\n", frame.sequence, frame.event, frame.data); err != nil {
				return nil
			}
			c.Response().Flush()
			if frame.terminal {
				return nil
			}
		case <-timer.C:
			if !setDeadline(time.Now().Add(5 * time.Second)) {
				return nil
			}
			if _, err := fmt.Fprint(c.Response(), ": heartbeat\n\n"); err != nil {
				return nil
			}
			c.Response().Flush()
			timer.Reset(dashboardJitter(20 * time.Second))
		}
	}
}

func (s *Service) dashboardPage(ctx context.Context, d *dashboardDelivery, sub *dashboardSubscription) (bool, error) {
	ctx = s.runDetailContext(ctx)
	c := s.echo.NewContext(sub.request.WithContext(ctx), &dashboardCredentialWriter{header: make(http.Header)})
	c.Set(hostedSharedClaimsKey, sub.claims)
	credential, _, err := s.appCredentialContext(ctx, c)
	if err != nil {
		return false, err
	}
	if credential.HostedRole != sub.scope.Credential.HostedRole {
		return false, hubapi.NativeNotFound()
	}
	scope := sub.scope
	scope.Credential = credential
	if s.config.TenantRepository == nil {
		return false, hubapi.NativeWorkItemStorageUnavailable()
	}
	if sub.board {
		return s.dashboardBoardRecordPage(ctx, d, sub, scope)
	}
	return s.dashboardProjectRecordPage(ctx, d, sub, scope)
}
