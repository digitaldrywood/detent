package hubserver

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func controlledHostedDelivery(t *testing.T, cfg dashboardDeliveryConfig, configure ...func(*Config)) (hostedSecurityFixture, hostedSecurityUser, *dashboardDelivery) {
	t.Helper()
	cfg.newTimer = func(time.Duration) dashboardTimer {
		return dashboardTimer{channel: make(chan time.Time), reset: func(time.Duration) {}, stop: func() {}}
	}
	configure = append([]func(*Config){func(c *Config) { c.dashboardDelivery = &cfg; c.Workspace = &WorkspaceConfig{Enabled: true} }}, configure...)
	f := newHostedSecurityFixture(t, configure...)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f.service.stopHealthDetector()
	f.service.stopIssueMailer()
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	f.service.dashboard.stop()
	d := newDashboardDelivery(f.service)
	f.service.dashboard = d
	user := f.user(t, "stream-member", "member", "stream@example.test", "write", "")
	return f, user, d
}

func hostedDeliveryHead(t *testing.T, f hostedSecurityFixture) int64 {
	t.Helper()
	head, _, err := readDashboardWatermark(t.Context(), f.service.database.reader, nativeScope{organization: "org_security", project: f.project})
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func appendHostedDeliveryLane(t *testing.T, f hostedSecurityFixture, name string) int64 {
	t.Helper()
	var seq int64
	if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		return tx.QueryRowContext(t.Context(), "INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id) VALUES('org_security',?,'lane',?) RETURNING sequence", f.project, name).Scan(&seq)
	}); err != nil {
		t.Fatal(err)
	}
	return seq
}

func pumpHostedDelivery(t *testing.T, d *dashboardDelivery) {
	t.Helper()
	if err := d.recover(t.Context(), d.service.database.reader); err != nil {
		t.Fatal(err)
	}
	d.changed()
	for d.remaining > 0 {
		d.deliver(t.Context())
	}
}

func readHostedCursorFrame(t *testing.T, r *bufio.Reader, event string, sequence int64) sseFrame {
	t.Helper()
	frame, err := readSSEFrame(r)
	if err != nil || frame.Event != event || frame.ID != strconv.FormatInt(sequence, 10) {
		t.Fatalf("frame=%+v err=%v, want %s/%d", frame, err, event, sequence)
	}
	return frame
}

func anotherHostedDeliveryProject(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) hostedSecurityFixture {
	t.Helper()
	other := f
	other.project = "prj_stream_other"
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) SELECT ?,organization_id,'Other project',profile,states_json,created_at,0 FROM projects WHERE id=?", other.project, f.project); err != nil {
		t.Fatal(err)
	}
	other.grant(t, user, false, false)
	return other
}

func TestHostedEventsReplay(t *testing.T) {
	t.Parallel()
	for _, header := range []bool{false, true} {
		t.Run(fmt.Sprintf("last-event-id=%t", header), func(t *testing.T) {
			f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{pageSize: 1})
			other := anotherHostedDeliveryProject(t, f, user)
			before := hostedDeliveryHead(t, f)
			first := appendHostedDeliveryLane(t, f, "first")
			otherSeq := appendHostedDeliveryLane(t, other, "other")
			second := appendHostedDeliveryLane(t, f, "second")
			third := appendHostedDeliveryLane(t, f, "third")
			stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
			otherStream := openHostedTestStream(t, other, user, "?since=0")
			pumpHostedDelivery(t, d)
			for _, seq := range []int64{first, second, third} {
				frame := readHostedCursorFrame(t, stream, "lane", seq)
				if strings.Contains(frame.Data, "other") {
					t.Fatal("cross-project content")
				}
			}
			readHostedCursorFrame(t, otherStream, "lane", otherSeq)
			query := "?since=" + strconv.FormatInt(first, 10)
			last := ""
			if header {
				query = "?since=0"
				last = strconv.FormatInt(first, 10)
			}
			replay := openHostedTestCursorStream(t, f, user, query, last)
			pumpHostedDelivery(t, d)
			readHostedCursorFrame(t, replay, "lane", second)
			readHostedCursorFrame(t, replay, "lane", third)
			d.mu.Lock()
			sub := d.subs.Back().Value.(*dashboardSubscription)
			d.mu.Unlock()
			more, err := f.service.dashboardPage(t.Context(), d, sub)
			if err != nil || more || len(sub.frames) != 0 || sub.sequence != third {
				t.Fatalf("duplicate replay: more=%t pending=%d seq=%d: %v", more, len(sub.frames), sub.sequence, err)
			}
		})
	}
}

func TestHostedEventsReset(t *testing.T) {
	t.Parallel()
	for _, test := range []string{"below floor", "ahead", "trim between pages"} {
		t.Run(test, func(t *testing.T) {
			f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{pageSize: 1, budget: 1})
			before := hostedDeliveryHead(t, f)
			first := appendHostedDeliveryLane(t, f, "first")
			head := appendHostedDeliveryLane(t, f, "second")
			since := before
			if test == "ahead" {
				since = head + 1
			}
			if test == "below floor" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_sequences SET replay_floor=? WHERE project_id=?", first, f.project); err != nil {
					t.Fatal(err)
				}
			}
			stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(since, 10))
			if test == "trim between pages" {
				d.changed()
				d.deliver(t.Context())
				readHostedCursorFrame(t, stream, "lane", first)
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_board_deltas WHERE project_id=? AND sequence<=?", f.project, head); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_sequences SET replay_floor=? WHERE project_id=?", head, f.project); err != nil {
					t.Fatal(err)
				}
			}
			pumpHostedDelivery(t, d)
			readHostedCursorFrame(t, stream, "reset", head)
			_, err := readSSEFrame(stream)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("reset did not close: %v", err)
			}

		})
	}
}

func TestHostedEventsAuthorizationCheckpointAndIdleRevocation(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{pageSize: 1})
	hidden := f.seedIssue(t, 1)
	reporter := f.user(t, "stream-reporter", "reporter", "reporter@example.test", "write", "")
	before := hostedDeliveryHead(t, f)
	var filtered int64
	if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		return tx.QueryRowContext(t.Context(), "INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision) VALUES('org_security',?,'work_item',?,99) RETURNING sequence", f.project, hidden).Scan(&filtered)
	}); err != nil {
		t.Fatal(err)
	}
	visible := appendHostedDeliveryLane(t, f, "visible")
	stream := openHostedTestStream(t, f, reporter, "?since="+strconv.FormatInt(before, 10))
	pumpHostedDelivery(t, d)
	frame := readHostedCursorFrame(t, stream, "checkpoint", filtered)
	if frame.Data != "{}" || strings.Contains(frame.Data, string(hidden)) {
		t.Fatalf("filtered content=%q", frame.Data)
	}
	readHostedCursorFrame(t, stream, "lane", visible)
	conn, err := f.service.database.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := d.recover(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	for d.remaining > 0 {
		d.deliver(t.Context())
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=?", reporter.identity.Subject); err != nil {
		t.Fatal(err)
	}
	if err := d.recover(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	for d.remaining > 0 {
		d.deliver(t.Context())
	}
	readHostedCursorFrame(t, stream, "reset", visible)
	_, err = readSSEFrame(stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("revoked stream=%v", err)
	}
	_ = user
}

func TestHostedEventsRecoveryAndAdmissionBudget(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{budget: 1, tenantLimit: 2})
	other := anotherHostedDeliveryProject(t, f, user)
	before := hostedDeliveryHead(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	otherStream := openHostedTestStream(t, other, user, "?since=0")
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events?since=0", nil), http.StatusTooManyRequests)
	pumpHostedDelivery(t, d)
	conn, err := f.service.database.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := d.recover(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	for d.remaining > 0 {
		d.deliver(t.Context())
	}
	first := appendHostedDeliveryLane(t, f, "first")
	second := appendHostedDeliveryLane(t, other, "second")
	select {
	case <-f.service.database.dashboardCommitted:
	default:
		t.Fatal("no commit notification")
	}
	if err := d.recover(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	d.deliver(t.Context())
	if d.remaining != 1 {
		t.Fatalf("recovery did not preserve budget continuation: %d", d.remaining)
	}
	d.deliver(t.Context())
	readHostedCursorFrame(t, stream, "lane", first)
	readHostedCursorFrame(t, otherStream, "lane", second)
}

func TestHostedEventsReplayLiveHandoff(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{pageSize: 1, budget: 1})
	before := hostedDeliveryHead(t, f)
	first := appendHostedDeliveryLane(t, f, "first")
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	barrier := make(chan struct{})
	committed := make(chan struct {
		sequence int64
		err      error
	}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-barrier:
		case <-ctx.Done():
			return
		}
		var seq int64
		err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
			return tx.QueryRowContext(ctx, "INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id) VALUES('org_security',?,'lane','handoff') RETURNING sequence", f.project).Scan(&seq)
		})
		committed <- struct {
			sequence int64
			err      error
		}{seq, err}
	}()
	t.Cleanup(func() { cancel(); <-done })
	var second int64
	var once sync.Once
	traced := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if strings.Contains(query, "SELECT sequence,topic,entity_id") {
			once.Do(func() {
				close(barrier)
				result := <-committed
				if result.err != nil {
					t.Fatal(result.err)
				}
				second = result.sequence
			})
		}
	}})
	d.changed()
	d.deliver(traced)
	readHostedCursorFrame(t, stream, "lane", first)
	if second <= first {
		t.Fatal("handoff barrier did not commit after the replay snapshot")
	}
	select {
	case <-f.service.database.dashboardCommitted:
	default:
		t.Fatal("lost commit wake")
	}

	pumpHostedDelivery(t, d)
	readHostedCursorFrame(t, stream, "lane", second)
	d.mu.Lock()
	sub := d.subs.Front().Value.(*dashboardSubscription)
	d.mu.Unlock()
	more, err := f.service.dashboardPage(t.Context(), d, sub)
	if err != nil || more || len(sub.frames) != 0 {
		t.Fatalf("handoff duplicated: more=%t queued=%d err=%v", more, len(sub.frames), err)
	}
}

func TestHostedEventsIdleHeartbeatReads(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{})
	other := anotherHostedDeliveryProject(t, f, user)
	head := hostedDeliveryHead(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	pumpHostedDelivery(t, d)
	conn, err := f.service.database.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := d.recover(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	for d.remaining > 0 {
		d.deliver(t.Context())
	}
	var queries atomic.Int64
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(string, []driver.NamedValue) { queries.Add(1) }})
	for range 3 {
		if err := d.recover(ctx, conn); err != nil {
			t.Fatal(err)
		}
		d.deliver(ctx)
	}
	if queries.Load() != 6 {
		t.Fatalf("idle subscription performed reads: %d, want only six shared recovery reads", queries.Load())
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET last_used_at=created_at"); err != nil {
		t.Fatal(err)
	}
	queries.Store(0)
	if err := d.recover(ctx, conn); err != nil {
		t.Fatal(err)
	}
	d.deliver(ctx)
	if queries.Load() != 2 {
		t.Fatalf("unrelated bookkeeping woke an idle stream: %d queries", queries.Load())
	}
	appendHostedDeliveryLane(t, other, "other-project")
	queries.Store(0)
	if err := d.recover(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for d.remaining > 0 {
		d.deliver(ctx)
	}
	if queries.Load() != 3 {
		t.Fatalf("other project delivery performed subscriber reads: %d queries", queries.Load())
	}
	queries.Store(0)
	heartbeat := make(chan time.Time, 1)
	d.config.newTimer = func(time.Duration) dashboardTimer {
		return dashboardTimer{channel: heartbeat, reset: func(time.Duration) {}, stop: func() {}}
	}
	second := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	heartbeat <- time.Now()
	frame, err := readSSEFrame(second)
	if err != nil || frame.ID != "" || frame.Event != "" || frame.Data != "" {
		t.Fatalf("heartbeat carries a cursor or content: %+v %v", frame, err)
	}
	if queries.Load() != 0 {
		t.Fatal("heartbeat read the database")
	}
	_ = stream
}

func TestHostedEventsSlowConsumerResumes(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{queueSize: 2, pageSize: 1, budget: 1})
	before := hostedDeliveryHead(t, f)
	slow := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	d.mu.Lock()
	sub := d.subs.Front().Value.(*dashboardSubscription)
	d.mu.Unlock()
	var sequences []int64
	for i := range 6 {
		sequences = append(sequences, appendHostedDeliveryLane(t, f, fmt.Sprintf("event-%d", i)))
	}
	pumpHostedDelivery(t, d)
	select {
	case <-sub.done:
	default:
		t.Fatal("slow consumer stayed open beyond queue bound")
	}
	if len(sub.frames) > 2 {
		t.Fatalf("queue exceeded bound: %d", len(sub.frames))
	}
	d.config.queueSize = 16
	replay := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	pumpHostedDelivery(t, d)
	for _, seq := range sequences {
		readHostedCursorFrame(t, replay, "lane", seq)
	}
	_ = slow
}

func TestHostedEventsTwoHubOwnersRecoverOtherWriter(t *testing.T) {
	t.Parallel()
	f, user, first := controlledHostedDelivery(t, dashboardDeliveryConfig{})
	committed := make(chan struct{}, 1)
	writer, err := openTimedWriter(sqliteWriterDSN(f.service.database.path, f.service.config.BusyTimeout, true), nil, committed)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := openTimedReader(sqliteReaderDSN(f.service.database.path, f.service.config.BusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	secondService := &Service{config: f.service.config, echo: echo.New(), hostedSessions: f.service.hostedSessions, database: &database{db: writer, reader: reader, authReader: reader, dashboardCommitted: committed, now: f.service.config.now}, secretRewrapReady: make(chan struct{})}
	secondService.ready.Store(true)
	second := newDashboardDelivery(secondService)
	secondService.dashboard = second
	secondService.echo.GET("/projects/:project/events", secondService.hostedEvents)
	secondFixture := f
	secondFixture.service = secondService
	head := hostedDeliveryHead(t, f)
	streamOne := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	streamTwo := openHostedTestStream(t, secondFixture, user, "?since="+strconv.FormatInt(head, 10))
	pumpHostedDelivery(t, first)
	pumpHostedDelivery(t, second)
	oneConn, err := f.service.database.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer oneConn.Close()
	twoConn, err := reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer twoConn.Close()
	for _, pair := range []struct {
		owner *dashboardDelivery
		conn  *sql.Conn
	}{{first, oneConn}, {second, twoConn}} {
		if err := pair.owner.recover(t.Context(), pair.conn); err != nil {
			t.Fatal(err)
		}
		for pair.owner.remaining > 0 {
			pair.owner.deliver(t.Context())
		}
	}
	for i, source := range []hostedSecurityFixture{f, secondFixture} {
		seq := appendHostedDeliveryLane(t, source, fmt.Sprintf("writer-%d", i))
		for _, pair := range []struct {
			owner *dashboardDelivery
			conn  *sql.Conn
		}{{first, oneConn}, {second, twoConn}} {
			if err := pair.owner.recover(t.Context(), pair.conn); err != nil {
				t.Fatal(err)
			}
			for pair.owner.remaining > 0 {
				pair.owner.deliver(t.Context())
			}
		}
		readHostedCursorFrame(t, streamOne, "lane", seq)
		readHostedCursorFrame(t, streamTwo, "lane", seq)
	}
}

func TestHostedEventsActionRunDelivery(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{})
	item := f.seedIssue(t, 1)
	var workspace workspacesession.Session
	response := f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{Mutation: tracker.Mutation{IdempotencyKey: "event-action-workspace"}, WorkItemID: string(item), Requires: []string{"files"}})
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &workspace)
	before := hostedDeliveryHead(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	now := f.service.config.now()
	var sequence int64
	if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		if err := insertProjectAction(t.Context(), tx, actionRecord{ID: "action_events", OrganizationID: "org_security", ProjectID: f.project, Name: "Example action", Command: "true", Icon: "play", CreatedBy: user.identity.Subject, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := insertProjectActionRun(t.Context(), tx, actionRunRecord{ID: "actionrun_events", ActionID: "action_events", OrganizationID: "org_security", ProjectID: f.project, WorkspaceID: workspace.ID, Command: "true", Status: "queued", CreatedBy: user.identity.Subject, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), "SELECT sequence FROM native_board_deltas WHERE topic='action_run' AND entity_id='actionrun_events'").Scan(&sequence)
	}); err != nil {
		t.Fatal(err)
	}
	pumpHostedDelivery(t, d)
	frame := readHostedCursorFrame(t, stream, "action_run.queued", sequence)
	var run workspacesession.Run
	if err := json.Unmarshal([]byte(frame.Data), &run); err != nil || run.ID != "actionrun_events" || run.ActionID != "action_events" || run.WorkspaceID != workspace.ID || run.Status != "queued" {
		t.Fatalf("run payload=%+v err=%v", run, err)
	}
}

func TestHostedEventsCommitWakeAndRecoveryTimer(t *testing.T) {
	t.Parallel()
	recovery := make(chan time.Time, 1)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	cfg := dashboardDeliveryConfig{newTimer: func(d time.Duration) dashboardTimer {
		var ch <-chan time.Time
		if d > 100*time.Millisecond && d < 2*time.Second {
			ch = recovery
		}
		return dashboardTimer{channel: ch, reset: func(time.Duration) {}, stop: func() {}}
	}}
	f := newHostedSecurityFixture(t, func(c *Config) {
		c.dashboardDelivery = &cfg
		c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	user := f.user(t, "timer-viewer", "viewer", "timer@example.test", "read", "")
	head := hostedDeliveryHead(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	first := appendHostedDeliveryLane(t, f, "commit-notification")
	readHostedCursorFrame(t, stream, "lane", first)
	clock.Add(int64(15 * time.Second))
	writer, err := sql.Open("sqlite", sqliteWriterDSN(f.service.database.path, f.service.config.BusyTimeout, true))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var second int64
	if err := writer.QueryRowContext(t.Context(), "INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id) VALUES('org_security',?,'lane','missed-notification') RETURNING sequence", f.project).Scan(&second); err != nil {
		t.Fatal(err)
	}
	recovery <- time.Now()
	readHostedCursorFrame(t, stream, "lane", second)
	if err := f.provider.RevokeSession(t.Context(), user.identity.Hosted.SessionID); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(6 * time.Second))
	recovery <- time.Now()
	readHostedCursorFrame(t, stream, "reset", second)
	_, err = readSSEFrame(stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("idle provider revocation did not close: %v", err)
	}
}

func TestHostedEventsLegacyBoardFiltersAndGap(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{})
	f.seedIssue(t, 1)
	f.seedIssue(t, 2)
	drainTestNativeBoard(t, f.service)
	head := hostedDeliveryHead(t, f)
	for _, test := range []struct {
		name, query string
		gap         bool
		total       int
	}{{"summary", "", false, 2}, {"filter", "&q=unmatched-example-term", false, 0}, {"gap", "", true, 2}} {
		t.Run(test.name, func(t *testing.T) {
			since := head
			if test.gap {
				since++
			}
			stream := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(since, 10)+test.query)
			pumpHostedDelivery(t, d)
			frame := readHostedCursorFrame(t, stream, "activity", head)
			var board tracker.NativeBoardFrame
			if err := json.Unmarshal([]byte(frame.Data), &board); err != nil {
				t.Fatal(err)
			}
			total := 0
			if board.Work == nil {
				t.Fatal("missing legacy summary")
			}
			for _, lane := range board.Work.Lanes {
				total += lane.Total
			}
			if board.Gap != test.gap || len(board.Deltas) != 0 || total != test.total || board.GrantVersion == "" {
				t.Fatalf("legacy frame=%+v total=%d want gap=%t total=%d", board, total, test.gap, test.total)
			}
		})
	}
}

func TestHostedEventsNotifyOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	committed := make(chan struct{}, 1)
	writer, err := openTimedWriter(sqliteWriterDSN(filepath.Join(t.TempDir(), "notifications.db"), time.Second, false), nil, committed)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.ExecContext(t.Context(), "CREATE TABLE events(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}

	for {
		select {
		case <-committed:
		default:
			goto drained
		}
	}
drained:
	for _, commit := range []bool{false, true} {
		tx, err := writer.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO events DEFAULT VALUES"); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		select {
		case <-committed:
			tx.Rollback()
			t.Fatal("transaction notified before commit")
		default:
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-committed:
			if !commit {
				t.Fatal("rollback notified delivery")
			}
		default:
			if commit {
				t.Fatal("committed change did not notify")
			}
		}
	}
	var id int64
	if err := writer.QueryRowContext(t.Context(), "INSERT INTO events DEFAULT VALUES RETURNING id").Scan(&id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-committed:
	default:
		t.Fatal("autocommit returning query did not notify")
	}

}

func TestHostedEventsLegacyBoardPagesShareSummary(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{pageSize: 1})
	f.seedIssue(t, 1)
	f.seedIssue(t, 2)
	before := hostedDeliveryHead(t, f)
	drainTestNativeBoard(t, f.service)
	head := hostedDeliveryHead(t, f)
	first := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(before, 10))
	second := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(before, 10))
	var summaries int
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
		if strings.Contains(query, "SELECT state,total,running FROM native_board_counts") {
			summaries++
		}
	}})
	d.changed()
	for d.remaining > 0 {
		d.deliver(ctx)
	}
	for _, stream := range []*bufio.Reader{first, second} {
		previous := before
		seen := map[tracker.NativeWorkItemID]bool{}
		for previous < head {
			frame, err := readSSEFrame(stream)
			if err != nil {
				t.Fatal(err)
			}
			var board tracker.NativeBoardFrame
			if err := json.Unmarshal([]byte(frame.Data), &board); err != nil {
				t.Fatal(err)
			}
			if board.Sequence <= previous || len(board.Deltas) > 1 || board.Gap || board.Work == nil {
				t.Fatalf("unbounded or non-resumable board frame=%+v", board)
			}
			for _, delta := range board.Deltas {
				if delta.Current == nil || seen[delta.Current.Issue.WorkItemID] {
					t.Fatalf("lost or duplicated board card: %+v", delta)
				}
				seen[delta.Current.Issue.WorkItemID] = true
			}
			previous = board.Sequence
		}
		if len(seen) != 2 {
			t.Fatalf("board replay lost cards: %d", len(seen))
		}
	}
	if summaries != 1 {
		t.Fatalf("summary recomputed per subscriber or page: %d", summaries)
	}
}

func TestHostedEventsDeletedEntityVisibility(t *testing.T) {
	t.Parallel()
	for _, entity := range []string{"item", "project"} {
		t.Run(entity, func(t *testing.T) {
			f, user, d := controlledHostedDelivery(t, dashboardDeliveryConfig{})
			item := f.seedIssue(t, 1)
			before := hostedDeliveryHead(t, f)
			query := "?since=" + strconv.FormatInt(before, 10)
			if entity == "item" {
				query += "&work_item=" + string(item)
			}
			stream := openHostedTestStream(t, f, user, query)
			pumpHostedDelivery(t, d)
			if entity == "item" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM issues WHERE native_id=?", item); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET deleted_at=? WHERE id=?", formatHubTime(f.service.config.now()), f.project); err != nil {
					t.Fatal(err)
				}
			}
			pumpHostedDelivery(t, d)
			readHostedCursorFrame(t, stream, "reset", before)
			if _, err := readSSEFrame(stream); !errors.Is(err, io.EOF) {
				t.Fatalf("deleted entity stream stayed open: %v", err)
			}
		})
	}
}

func TestHostedEventsConcurrentDisconnect(t *testing.T) {
	t.Parallel()
	var aborted atomic.Int64
	sub := &dashboardSubscription{frames: make(chan dashboardFrame, 8), done: make(chan struct{}), abort: func() { aborted.Add(1) }}
	start := make(chan struct{})
	var work sync.WaitGroup
	for i := range 4 {
		work.Go(func() {
			<-start
			for range 100 {
				if i%2 == 0 {
					sub.close()
				} else {
					sub.enqueue(dashboardFrame{sequence: 1, event: "lane", data: "{}"})
				}
			}
		})
	}
	close(start)
	work.Wait()
	if aborted.Load() != 1 || sub.enqueue(dashboardFrame{}) {
		t.Fatalf("disconnect aborts=%d, closed=%t", aborted.Load(), sub.closed.Load())
	}
	select {
	case <-sub.done:
	default:
		t.Fatal("disconnect did not release the handler")
	}
}

type failedDashboardDeadlineWriter struct {
	*httptest.ResponseRecorder
	written chan struct{}
}

func (w failedDashboardDeadlineWriter) SetWriteDeadline(time.Time) error {
	return errors.New("write deadline unavailable")
}

func (w failedDashboardDeadlineWriter) Write(p []byte) (int, error) {
	select {
	case w.written <- struct{}{}:
	default:
	}
	return w.ResponseRecorder.Write(p)
}

func TestHostedEventsWriteDeadlineFailureClosesBeforeWrite(t *testing.T) {
	t.Parallel()
	for _, heartbeat := range []bool{false, true} {
		t.Run(fmt.Sprintf("heartbeat=%t", heartbeat), func(t *testing.T) {
			ready := make(chan struct{})
			tick := make(chan time.Time, 1)
			cfg := dashboardDeliveryConfig{newTimer: func(time.Duration) dashboardTimer {
				close(ready)
				return dashboardTimer{channel: tick, reset: func(time.Duration) {}, stop: func() {}}
			}}
			s := &Service{config: Config{Logger: discardLogger(), dashboardDelivery: &cfg}}
			s.dashboard = newDashboardDelivery(s)
			ctx, cancel := context.WithCancel(t.Context())
			writer := failedDashboardDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{}, 1)}
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx), writer)
			result := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				result <- s.serveDashboardEvents(c, nativeScope{organization: "org_example"}, 0, false, false, "", "")
			}()
			t.Cleanup(func() { cancel(); <-finished })
			<-ready
			if heartbeat {
				tick <- time.Now()
			} else {
				s.dashboard.mu.Lock()
				sub := s.dashboard.subs.Front().Value.(*dashboardSubscription)
				s.dashboard.mu.Unlock()
				sub.enqueue(dashboardFrame{sequence: 1, event: "lane", data: "{}"})
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-writer.written:
				t.Fatal("stream wrote after failing to install its write deadline")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			<-finished
			if writer.Body.Len() != 0 || s.dashboard.subs.Len() != 0 || len(s.dashboard.tenants) != 0 {
				t.Fatal("failed write deadline left output or a registered subscription")
			}
		})
	}
}
