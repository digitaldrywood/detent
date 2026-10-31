package hubserver

import (
	"container/list"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

type dashboardCredentialWriter struct{ header http.Header }

func (w *dashboardCredentialWriter) Header() http.Header         { return w.header }
func (w *dashboardCredentialWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *dashboardCredentialWriter) WriteHeader(int)             {}

type dashboardTimer struct {
	channel <-chan time.Time
	reset   func(time.Duration)
	stop    func()
}

type dashboardDeliveryConfig struct {
	newTimer    func(time.Duration) dashboardTimer
	pageSize    int
	queueSize   int
	tenantLimit int
	budget      int
}

func defaultDashboardTimer(d time.Duration) dashboardTimer {
	t := time.NewTimer(d)
	return dashboardTimer{channel: t.C, reset: func(d time.Duration) { t.Reset(d) }, stop: func() { t.Stop() }}
}

func dashboardJitter(d time.Duration) time.Duration {
	return d - d/10 + time.Duration(float64(d/5)*randomJitter())
}

type dashboardFrame struct {
	sequence int64
	event    string
	data     string
	terminal bool
}

type dashboardSubscription struct {
	request       *http.Request
	claims        any
	scope         nativeScope
	params        url.Values
	workspace     string
	item          string
	board         bool
	initial       bool
	sequence      int64
	grantVersion  string
	epoch         uint64
	authorization uint64
	more          bool
	terminal      bool
	frames        chan dashboardFrame
	done          chan struct{}
	abort         func()
	closed        atomic.Bool
	closeOnce     sync.Once
	element       *list.Element
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

type dashboardDelivery struct {
	service        *Service
	config         dashboardDeliveryConfig
	mu             sync.Mutex
	subs           list.List
	tenants        map[tracker.OrganizationID]int
	closed         bool
	wake           chan struct{}
	cancel         context.CancelFunc
	done           chan struct{}
	epoch          uint64
	remaining      int
	version        int64
	authorization  uint64
	watermarks     map[string]int64
	head           int64
	authAt         time.Time
	boardCache     map[string]dashboardFrame
	boardSummaries map[string]*tracker.NativeWorkSummary
}

func newDashboardDelivery(s *Service) *dashboardDelivery {
	cfg := dashboardDeliveryConfig{newTimer: defaultDashboardTimer, pageSize: 64, queueSize: 128, tenantLimit: 128, budget: 32}
	if override := s.config.dashboardDelivery; override != nil {
		if override.newTimer != nil {
			cfg.newTimer = override.newTimer
		}
		if override.pageSize > 0 {
			cfg.pageSize = override.pageSize
		}
		if override.queueSize > 0 {
			cfg.queueSize = override.queueSize
		}
		if override.tenantLimit > 0 {
			cfg.tenantLimit = override.tenantLimit
		}
		if override.budget > 0 {
			cfg.budget = override.budget
		}
	}
	return &dashboardDelivery{service: s, config: cfg, tenants: make(map[tracker.OrganizationID]int), wake: make(chan struct{}, 1), done: make(chan struct{}), boardCache: make(map[string]dashboardFrame), boardSummaries: make(map[string]*tracker.NativeWorkSummary), watermarks: make(map[string]int64)}
}

func (d *dashboardDelivery) start() {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	go d.run(ctx)
}

func (d *dashboardDelivery) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	if d.cancel != nil {
		d.cancel()
		<-d.done
	}
}

func (d *dashboardDelivery) add(sub *dashboardSubscription) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.tenants[sub.scope.organization] >= d.config.tenantLimit {
		return false
	}
	sub.frames = make(chan dashboardFrame, d.config.queueSize)
	sub.done = make(chan struct{})
	sub.more = true
	sub.element = d.subs.PushBack(sub)
	d.tenants[sub.scope.organization]++
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
		d.subs.Remove(sub.element)
		sub.element = nil
		d.tenants[sub.scope.organization]--
		if d.tenants[sub.scope.organization] == 0 {
			delete(d.tenants, sub.scope.organization)
		}
	}
}

func (d *dashboardDelivery) changed() {
	d.epoch++
	clear(d.watermarks)
	clear(d.boardCache)
	clear(d.boardSummaries)
	d.mu.Lock()
	d.remaining = d.subs.Len()
	d.mu.Unlock()
}

func (d *dashboardDelivery) recover(ctx context.Context, conn nativeQueryer) error {
	var version, head int64
	if err := conn.QueryRowContext(ctx, "SELECT sequence FROM dashboard_delivery_authorization").Scan(&version); err != nil {
		return fmt.Errorf("read dashboard authorization watermark: %w", err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM native_board_deltas").Scan(&head); err != nil {
		return fmt.Errorf("read dashboard delivery head: %w", err)
	}
	now := d.service.config.now()
	authorizationChanged := version != d.version || !now.Before(d.authAt)
	if authorizationChanged || head != d.head {
		if authorizationChanged {
			d.authorization++
			d.authAt = now.Add(20 * time.Second)
		}
		d.version, d.head = version, head
		d.changed()
	}
	return nil
}

func (d *dashboardDelivery) run(ctx context.Context) {
	defer close(d.done)
	defer func() {
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
	timer := d.config.newTimer(dashboardJitter(time.Second))
	timer.stop()
	var recoveryTicks <-chan time.Time
	defer timer.stop()
	continuation := d.config.newTimer(dashboardJitter(10 * time.Millisecond))
	continuation.stop()
	var continuationTicks <-chan time.Time
	defer continuation.stop()
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
			timer.reset(dashboardJitter(time.Second))
		}
		d.mu.Lock()
		active := d.subs.Len() > 0
		d.mu.Unlock()
		if !active {
			timer.stop()
			continuation.stop()
			recoveryTicks, continuationTicks = nil, nil
			continue
		}
		if recoveryTicks == nil {
			recoveryTicks = timer.channel
			timer.reset(dashboardJitter(time.Second))
			recoverTick = true
		}
		if recoverTick {
			readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
			err := d.recover(readCtx, d.service.database.reader)
			cancel()
			if err != nil {
				if ctx.Err() == nil {
					d.service.config.Logger.ErrorContext(ctx, "recover dashboard delivery", "error", err)
				}
				continue
			}
		}

		d.deliver(ctx)
		if d.remaining > 0 {
			continuation.reset(dashboardJitter(10 * time.Millisecond))
			continuationTicks = continuation.channel
		} else {
			continuation.stop()
			continuationTicks = nil
		}
	}
}

func (d *dashboardDelivery) deliver(ctx context.Context) {
	for range d.config.budget {
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
		readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
		if !sub.more && sub.authorization == d.authorization {
			key := string(sub.scope.organization) + "/" + string(sub.scope.project)
			head, ok := d.watermarks[key]
			if !ok {
				var err error
				head, _, err = readDashboardWatermark(readCtx, d.service.database.reader, sub.scope)
				if err != nil {
					cancel()
					sub.reset(sub.sequence)
					continue
				}
				if len(d.watermarks) < 512 {
					d.watermarks[key] = head
				}
			}
			if sub.sequence == head {
				sub.epoch = d.epoch
				cancel()
				continue
			}
		}
		more, err := d.service.dashboardPage(readCtx, d, sub)
		cancel()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
				d.service.config.Logger.ErrorContext(ctx, "deliver dashboard page", "error", err)
			}
			sub.reset(sub.sequence)
			more = false
		}
		sub.epoch, sub.more, sub.authorization = d.epoch, more, d.authorization
		if more {
			d.mu.Lock()
			d.remaining = max(d.remaining, d.subs.Len())
			d.mu.Unlock()
		}
	}
}

func (s *Service) serveDashboardEvents(c echo.Context, scope nativeScope, sequence int64, initial, board bool, workspace, item string) error {
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
	timer := d.config.newTimer(dashboardJitter(20 * time.Second))
	defer timer.stop()
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
		case <-timer.channel:
			if !setDeadline(time.Now().Add(5 * time.Second)) {
				return nil
			}
			if _, err := fmt.Fprint(c.Response(), ": heartbeat\n\n"); err != nil {
				return nil
			}
			c.Response().Flush()
			timer.reset(dashboardJitter(20 * time.Second))
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
	if credential.HostedRole != sub.scope.credential.HostedRole {
		return false, nativeNotFound()
	}
	scope := sub.scope
	scope.credential = credential
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := s.requireHostedProject(ctx, tx, scope, false); err != nil {
		return false, err
	}
	if err := requireDashboardProject(ctx, tx, scope); err != nil {
		return false, err
	}
	if sub.item != "" {
		visible, err := dashboardItemVisible(ctx, tx, scope, sub.item)
		if err != nil {
			return false, err
		}
		if !visible {
			return false, nativeNotFound()
		}
	}
	if sub.workspace != "" {
		if s.workspaces == nil {
			return false, nativeNotFound()
		}
		if _, err := readDashboardWorkspace(ctx, tx, scope, sub.workspace); err != nil {
			return false, err
		}
	}
	if sub.board {
		return s.dashboardBoardPage(ctx, tx, d, sub, scope)
	}
	page, err := readDashboardChanges(ctx, tx, scope, sub.sequence, d.config.pageSize)
	if err != nil {
		return false, err
	}
	if page.Gap {
		sub.reset(page.Sequence)
		return false, nil
	}
	for _, change := range page.Changes {
		frame, visible, err := s.dashboardChangeFrame(ctx, tx, scope, sub, change)
		if err != nil {
			return false, err
		}
		if !visible {
			frame = dashboardFrame{sequence: change.Sequence, event: "checkpoint", data: "{}"}
		}
		if !sub.enqueue(frame) {
			return false, nil
		}
		sub.sequence = change.Sequence
	}
	if sub.initial && len(page.Changes) == 0 {
		sub.enqueue(dashboardFrame{sequence: sub.sequence, event: "checkpoint", data: "{}"})
	}
	sub.initial = false
	return page.NextSequence < page.Sequence, nil
}

func (s *Service) dashboardBoardPage(ctx context.Context, tx *sql.Tx, d *dashboardDelivery, sub *dashboardSubscription, scope nativeScope) (bool, error) {
	page, err := readDashboardChanges(ctx, tx, scope, sub.sequence, d.config.pageSize)
	if err != nil {
		return false, err
	}
	head := page.Sequence
	grants, err := nativeBoardGrants(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	grant := nativeBoardGrantVersion(grants)
	if !sub.initial && sub.sequence == head && sub.grantVersion == grant {
		return false, nil
	}
	summaryKey := fmt.Sprintf("%s/%s/%s/%s/%s/%d/%s", scope.organization, scope.project, appSubject(scope.credential), scope.credential.HostedRole, grant, head, sub.params.Encode())
	key := fmt.Sprintf("%s/%d/%d", summaryKey, sub.sequence, page.NextSequence)
	frame, ok := d.boardCache[key]
	if !ok {
		board, err := readNativeBoardFrameThrough(ctx, tx, scope, sub.sequence, page.NextSequence)
		if err != nil {
			return false, err
		}
		board.Work = d.boardSummaries[summaryKey]
		if board.Work == nil {
			board.Work, err = readNativeBoardSummary(ctx, tx, scope, sub.params, s.config.now())
			if err != nil {
				return false, err
			}
			if len(d.boardSummaries) < d.config.tenantLimit {
				d.boardSummaries[summaryKey] = board.Work
			}
		}
		for i := range board.Deltas {
			if board.Deltas[i].Current != nil {
				board.Deltas[i].Current.Issue = s.nativeIssueResponse(board.Deltas[i].Current.Issue)
			}
		}
		raw, err := json.Marshal(board)
		if err != nil {
			return false, err
		}
		frame = dashboardFrame{sequence: board.Sequence, event: "activity", data: string(raw)}
		if len(d.boardCache) < d.config.tenantLimit {
			d.boardCache[key] = frame
		}
	}
	sub.enqueue(frame)
	sub.sequence, sub.initial, sub.grantVersion = frame.sequence, false, grant
	return !page.Gap && frame.sequence < head, nil
}

func (s *Service) dashboardChangeFrame(ctx context.Context, q nativeQueryer, scope nativeScope, sub *dashboardSubscription, change dashboardChange) (dashboardFrame, bool, error) {
	frame := dashboardFrame{sequence: change.Sequence, event: change.Topic, data: "{}"}
	item := ""
	switch change.Topic {
	case "work_item":
		item = change.EntityID
	case "activity":
		item = change.RelatedID
	case "human_action":
		item = change.RelatedID
		if item == "" {
			item = change.EntityID
		}
	case "board":
		if scope.credential.HostedRole == "reporter" {
			return frame, false, nil
		}
	}
	if item != "" {
		visible, err := dashboardItemVisible(ctx, q, scope, item)
		if err != nil || !visible {
			return frame, false, err
		}
	}
	if sub.item != "" && item != sub.item && change.Topic != "workspace" && change.Topic != "action_run" {
		return frame, false, nil
	}
	if sub.workspace != "" && change.Topic != "workspace" && change.Topic != "action_run" {
		return frame, false, nil
	}
	switch change.Topic {
	case "workspace":
		if s.workspaces == nil || sub.workspace != "" && sub.workspace != change.EntityID || sub.item != "" && sub.item != change.RelatedID {
			return frame, false, nil
		}
		record, err := readDashboardWorkspace(ctx, q, scope, change.EntityID)
		if isNativeNotFound(err) || errors.Is(err, sql.ErrNoRows) {
			return frame, false, nil
		}
		if err != nil {
			return frame, false, err
		}
		frame.event = workspacesession.EventType(change.Status)
		resource := s.workspaces.presentWorkspace(ctx, q, scope, record)
		resource.State, resource.Revision = change.Status, change.EntityRevision
		raw, err := json.Marshal(struct {
			workspacesession.Session
			ChangeSequence int64 `json:"change_sequence"`
		}{resource, change.Sequence})
		frame.data = string(raw)
		return frame, true, err
	case "action_run":
		if s.workspaces == nil || sub.workspace != "" && sub.workspace != change.RelatedID {
			return frame, false, nil
		}
		workspace, err := readDashboardWorkspace(ctx, q, scope, change.RelatedID)
		if isNativeNotFound(err) || errors.Is(err, sql.ErrNoRows) {
			return frame, false, nil
		}
		if err != nil {
			return frame, false, err
		}
		if sub.item != "" && sub.item != workspace.SubjectWorkItemID {
			return frame, false, nil
		}
		run, err := readProjectActionRunByID(ctx, q, change.EntityID)
		if errors.Is(err, sql.ErrNoRows) {
			return frame, false, nil
		}
		if err != nil {
			return frame, false, err
		}
		if run.OrganizationID != scope.organization || run.ProjectID != scope.project {
			return frame, false, nil
		}
		resource := run.resource()
		resource.Status, resource.Revision = change.Status, change.EntityRevision
		raw, err := json.Marshal(resource)
		frame.event, frame.data = "action_run."+change.Status, string(raw)
		return frame, true, err
	case "health":
		if scope.credential.HostedRole == "reporter" {
			return frame, false, nil
		}
		frame.event = "health.findings"
	case "activity":
		frame.data = strconv.FormatInt(change.Sequence, 10)
		return frame, true, nil
	}
	raw, err := json.Marshal(change)
	frame.data = string(raw)
	return frame, true, err
}

func requireDashboardProject(ctx context.Context, q nativeQueryer, scope nativeScope) error {
	if err := authorizeNativeProject(ctx, q, scope); err != nil {
		return err
	}
	var active bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM projects WHERE organization_id=? AND id=? AND deleted_at IS NULL)", scope.organization, scope.project).Scan(&active); err != nil {
		return fmt.Errorf("read dashboard project: %w", err)
	}
	if !active {
		return nativeNotFound()
	}
	return nil
}

func dashboardItemVisible(ctx context.Context, q nativeQueryer, scope nativeScope, id string) (bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT actor_json FROM issues WHERE organization_id=? AND project_id=? AND native_id=?", scope.organization, scope.project, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read dashboard item visibility: %w", err)
	}
	if scope.credential.HostedRole != "reporter" {
		return true, nil
	}
	var actor tracker.Actor
	if err := json.Unmarshal([]byte(raw), &actor); err != nil {
		return false, fmt.Errorf("decode dashboard item actor: %w", err)
	}
	return scope.reporterOwns(actor), nil
}

func readDashboardWorkspace(ctx context.Context, q nativeQueryer, scope nativeScope, id string) (workspaceRecord, error) {
	if err := workspacesession.ValidateID(id); err != nil {
		return workspaceRecord{}, nativeNotFound()
	}
	record, err := readWorkspaceRow(ctx, q, scope, id)
	if errors.Is(err, sql.ErrNoRows) {
		return workspaceRecord{}, nativeNotFound()
	}
	if err != nil {
		return workspaceRecord{}, err
	}
	visible, err := dashboardItemVisible(ctx, q, scope, record.SubjectWorkItemID)
	if err != nil {
		return workspaceRecord{}, err
	}
	if !visible {
		return workspaceRecord{}, nativeNotFound()
	}
	return record, nil
}
