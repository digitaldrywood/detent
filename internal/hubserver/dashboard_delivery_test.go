package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/actionstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func controlledHostedDelivery(t *testing.T, cfg hubdeps.DashboardDelivery, configure ...func(*Config)) (hostedSecurityFixture, hostedSecurityUser, *dashboardDelivery) {
	t.Helper()
	cfg.NewTimer = func(time.Duration) hubdeps.Timer {
		return hubdeps.Timer{C: make(chan time.Time), Reset: func(time.Duration) {}, Stop: func() {}}
	}
	configure = append([]func(*Config){func(c *Config) { c.Deps().DashboardDelivery = &cfg; c.Workspace = &WorkspaceConfig{Enabled: true} }}, configure...)
	f := newHostedSecurityFixture(t, configure...)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
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
	return dashboardPostgresHead(t, f)
}

func pumpHostedDelivery(t *testing.T, d *dashboardDelivery) {
	t.Helper()
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

func readHostedItemFrames(t *testing.T, r *bufio.Reader, sequence int64) sseFrame {
	t.Helper()
	frame := readHostedCursorFrame(t, r, "work_item", sequence-1)
	readHostedCursorFrame(t, r, "human_action", sequence)
	return frame
}

func TestHostedEventsReplay(t *testing.T) {
	t.Parallel()
	for _, header := range []bool{false, true} {
		t.Run(fmt.Sprintf("last-event-id=%t", header), func(t *testing.T) {
			f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{PageSize: 1})
			other := dashboardPostgresProject(t, f, user)
			before := dashboardPostgresHead(t, f)
			first := dashboardPostgresAppend(t, f, "first")
			otherSeq := dashboardPostgresAppend(t, other, "other")
			second := dashboardPostgresAppend(t, f, "second")
			third := dashboardPostgresAppend(t, f, "third")
			stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
			otherStream := openHostedTestStream(t, other, user, "?since=0")
			pumpHostedDelivery(t, d)
			for _, seq := range []int64{first, second, third} {
				frame := readHostedItemFrames(t, stream, seq)
				if strings.Contains(frame.Data, "other") {
					t.Fatal("cross-project content")
				}
			}
			readHostedItemFrames(t, otherStream, otherSeq)
			query := "?since=" + strconv.FormatInt(first, 10)
			last := ""
			if header {
				query = "?since=0"
				last = strconv.FormatInt(first, 10)
			}
			replay := openHostedTestCursorStream(t, f, user, query, last)
			pumpHostedDelivery(t, d)
			readHostedItemFrames(t, replay, second)
			readHostedItemFrames(t, replay, third)
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
			f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{PageSize: 1, Budget: 1})
			before := dashboardPostgresHead(t, f)
			first := dashboardPostgresAppend(t, f, "first")
			head := dashboardPostgresAppend(t, f, "second")
			since := before
			if test == "ahead" {
				since = head + 1
			}
			if test == "below floor" {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE native_board_sequences SET replay_floor=$1 WHERE organization_id='org_security' AND project_id=$2", first, f.project); err != nil {
					t.Fatal(err)
				}
			}
			stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(since, 10))
			if test == "trim between pages" {
				d.changed()
				d.deliver(t.Context())
				readHostedCursorFrame(t, stream, "work_item", first-1)
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM native_board_deltas WHERE organization_id='org_security' AND project_id=$1 AND sequence<=$2", f.project, head); err != nil {
					t.Fatal(err)
				}
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE native_board_sequences SET replay_floor=$1 WHERE organization_id='org_security' AND project_id=$2", head, f.project); err != nil {
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
	f, _, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{PageSize: 1})
	hidden := dashboardPostgresIssue(t, f, 1)
	reporter := f.user(t, "stream-reporter", "reporter", "reporter@example.test", "", "")
	dashboardPostgresGrant(t, f, reporter)
	before := dashboardPostgresHead(t, f)
	repository := f.service.config.TenantRepository
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		item, err := w.WorkItem(t.Context(), f.project, hidden)
		if err != nil {
			return err
		}
		item.Title = "Hidden current source"
		_, err = w.UpdateWorkItem(t.Context(), item)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	filtered := dashboardPostgresHead(t, f)
	if filtered != before+1 {
		t.Fatalf("hidden source notice=%d want %d", filtered, before+1)
	}
	visible := appendDashboardPostgresLane(t, f)
	stream := openHostedTestStream(t, f, reporter, "?since="+strconv.FormatInt(before, 10))
	pumpHostedDelivery(t, d)
	frame := readHostedCursorFrame(t, stream, "checkpoint", filtered)
	if frame.Data != "{}" || strings.Contains(frame.Data, string(hidden)) {
		t.Fatalf("filtered content=%q", frame.Data)
	}
	readHostedCursorFrame(t, stream, "lane", visible)
	pumpHostedDelivery(t, d)
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), reporter.identity.Subject, f.project)
	}); err != nil {
		t.Fatal(err)
	}
	if head := dashboardPostgresHead(t, f); head != visible {
		t.Fatalf("grant revocation changed project head: %d want %d", head, visible)
	}
	pumpHostedDelivery(t, d)
	readHostedCursorFrame(t, stream, "reset", visible)
	if _, err := readSSEFrame(stream); !errors.Is(err, io.EOF) {
		t.Fatalf("revoked stream=%v", err)
	}
}

func TestHostedEventsRecoveryAndAdmissionBudget(t *testing.T) {
	t.Parallel()
	f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{Budget: 1, TenantLimit: 2})
	other := dashboardPostgresProject(t, f, user)
	before := dashboardPostgresHead(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	otherStream := openHostedTestStream(t, other, user, "?since=0")
	requireNativeStatus(t, f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events?since=0", nil), http.StatusTooManyRequests)
	pumpHostedDelivery(t, d)
	select {
	case <-f.service.database.dashboardCommitted:
	default:
	}
	first := appendDashboardPostgresLane(t, f)
	second := appendDashboardPostgresLane(t, other)
	select {
	case <-f.service.database.dashboardCommitted:
	default:
		t.Fatal("no commit notification")
	}
	d.changed()
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
	f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{PageSize: 1, Budget: 1})
	before := dashboardPostgresHead(t, f)
	first := appendDashboardPostgresLane(t, f)
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	select {
	case <-f.service.database.dashboardCommitted:
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	barrier := make(chan struct{})
	committed := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-barrier:
		case <-ctx.Done():
			return
		}
		committed <- f.service.config.TenantRepository.WriteTenant(ctx, "org_security", corestore.Background, func(w tenantstore.Writer) error {
			return dashboardPostgresLane(ctx, w, f.project)
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	var once sync.Once
	traced := postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
		if strings.Contains(query, "FROM native_board_deltas WHERE") {
			once.Do(func() {
				close(barrier)
				select {
				case err := <-committed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	})
	d.changed()
	d.deliver(traced)
	readHostedCursorFrame(t, stream, "lane", first)
	second := dashboardPostgresHead(t, f)
	if second != first+1 {
		t.Fatal("handoff barrier did not commit one notice after the replay snapshot")
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

func TestHostedEventsIdleHeartbeatHasNoCursor(t *testing.T) {
	t.Parallel()
	f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{})
	head := dashboardPostgresHead(t, f)
	query := "?since=" + strconv.FormatInt(head, 10)
	heartbeat := make(chan time.Time, 1)
	d.config.NewTimer = func(time.Duration) hubdeps.Timer {
		return hubdeps.Timer{C: heartbeat, Reset: func(time.Duration) {}, Stop: func() {}}
	}
	heartbeatStream := openHostedTestStream(t, f, user, query)
	heartbeat <- time.Now()
	frame, err := readSSEFrame(heartbeatStream)
	if err != nil || frame.ID != "" || frame.Event != "" || frame.Data != "" {
		t.Fatalf("heartbeat carries a cursor or content: %+v %v", frame, err)
	}
}

func TestHostedEventsSlowConsumerResumes(t *testing.T) {
	t.Parallel()
	f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{QueueSize: 2, PageSize: 1, Budget: 1})
	before := dashboardPostgresHead(t, f)
	slow := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	d.mu.Lock()
	sub := d.subs.Front().Value.(*dashboardSubscription)
	d.mu.Unlock()
	var sequences []int64
	for i := range 6 {
		sequences = append(sequences, dashboardPostgresAppend(t, f, fmt.Sprintf("event-%d", i)))
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
	d.config.QueueSize = 16
	replay := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
	pumpHostedDelivery(t, d)
	for _, seq := range sequences {
		readHostedItemFrames(t, replay, seq)
	}
	_ = slow
}

func TestHostedEventsTwoHubOwnersRecoverOtherWriter(t *testing.T) {
	t.Parallel()
	f, user, first := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{})
	secondService := &Service{config: f.service.config, echo: echo.New(), hostedSessions: f.service.hostedSessions, database: &database{postgres: f.service.database.postgres, billingRepository: f.service.database.billingRepository, now: f.service.config.Clock}, secretRewrapReady: make(chan struct{})}
	secondService.ready.Store(true)
	second := newDashboardDelivery(secondService)
	secondService.dashboard = second
	secondService.echo.GET("/projects/:project/events", secondService.hostedEvents)
	t.Cleanup(second.stop)
	secondFixture := f
	secondFixture.service = secondService
	head := dashboardPostgresHead(t, f)
	streamOne := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	streamTwo := openHostedTestStream(t, secondFixture, user, "?since="+strconv.FormatInt(head, 10))
	pumpHostedDelivery(t, first)
	pumpHostedDelivery(t, second)
	for i, source := range []hostedSecurityFixture{f, secondFixture} {
		seq := dashboardPostgresAppend(t, source, fmt.Sprintf("writer-%d", i))
		for _, owner := range []*dashboardDelivery{first, second} {
			owner.changed()
			for owner.remaining > 0 {
				owner.deliver(t.Context())
			}
		}
		readHostedItemFrames(t, streamOne, seq)
		readHostedItemFrames(t, streamTwo, seq)
	}
}

func TestHostedEventsActionRunDelivery(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, hubdeps.DashboardDelivery{})
	item := f.seedIssue(t, 1)
	var workspace workspacesession.Session
	response := f.request(t, user, http.MethodPost, f.base+"/workspaces", workspaceRequest{Mutation: tracker.Mutation{IdempotencyKey: "event-action-workspace"}, WorkItemID: string(item), Requires: []string{"files"}})
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &workspace)
	before := hostedDeliveryHead(t, f)
	stream := openHostedTestStream(t, f, user, "?workspace="+workspace.ID+"&since="+strconv.FormatInt(before, 10))
	now := f.service.config.Clock()
	var sequence, workspaceSequence int64
	command, err := f.service.runDetailStore().StoreDetail(t.Context(), "org_security", []byte("true"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		workspaceOwner := w.(postgres.WorkspaceWriter)
		current, err := workspaceOwner.WorkspaceSession(t.Context(), f.project, workspace.ID)
		if err != nil {
			return err
		}
		if _, err := workspaceOwner.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: current.OrganizationID, ProjectID: current.ProjectID, ID: current.ID, ExpectedRevision: current.Revision, ExpectedRunnerID: current.RunnerID, ExpectedLeaseID: current.LeaseID, ExpectedFencingToken: current.FencingToken, State: workspacesession.StateStarting, UpdatedAt: now}); err != nil {
			return err
		}
		owner := w.(postgres.RelayActionWriter)
		if err := owner.CreateProjectAction(t.Context(), actionstore.Record{Action: workspacesession.Action{ID: "action_events", OrganizationID: "org_security", ProjectID: string(f.project), Name: "Example action", Icon: "play", CreatedBy: user.identity.Subject, Revision: 1, CreatedAt: now, UpdatedAt: now}, Command: command}); err != nil {
			return err
		}
		if _, err := owner.CreateActionRun(t.Context(), postgres.CreateActionRunInput{ID: "actionrun_events", ActionID: "action_events", OrganizationID: "org_security", ProjectID: f.project, WorkspaceID: workspace.ID, CreatedBy: user.identity.Subject, CreatedAt: now}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		rows, err := r.BoardDeltas(t.Context(), f.project, before, corestore.MaxPageSize)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Topic == "action_run" && row.EntityID == "actionrun_events" {
				sequence = row.Sequence
			}
			if row.Topic == "workspace" && row.EntityID == workspace.ID {
				workspaceSequence = row.Sequence
			}
		}
		if sequence == 0 || workspaceSequence == 0 || workspaceSequence >= sequence {
			return fmt.Errorf("dashboard deltas workspace=%d action run=%d", workspaceSequence, sequence)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pumpHostedDelivery(t, d)
	readHostedCursorFrame(t, stream, workspacesession.EventType(workspacesession.StateStarting), workspaceSequence)
	frame := readHostedCursorFrame(t, stream, "action_run.queued", sequence)
	var run workspacesession.Run
	if err := json.Unmarshal([]byte(frame.Data), &run); err != nil || run.ID != "actionrun_events" || run.ActionID != "action_events" || run.WorkspaceID != workspace.ID || run.Status != "queued" {
		t.Fatalf("run payload=%+v err=%v", run, err)
	}
}

func TestHostedEventsCommitWakeAndRecoveryTimer(t *testing.T) {
	t.Parallel()
	recovery := make(chan time.Time, 1)
	cfg := hubdeps.DashboardDelivery{NewTimer: func(d time.Duration) hubdeps.Timer {
		var ch <-chan time.Time
		if d > 100*time.Millisecond && d < 2*time.Second {
			ch = recovery
		}
		return hubdeps.Timer{C: ch, Reset: func(time.Duration) {}, Stop: func() {}}
	}}
	f, _, d := controlledPostgresDelivery(t, cfg)
	user := f.user(t, "timer-viewer", "viewer", "timer@example.test", "", "")
	dashboardPostgresGrant(t, f, user)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f.service.config.Clock = func() time.Time { return time.Unix(0, clock.Load()) }
	head := dashboardPostgresHead(t, f)
	d.start()
	stream := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(head, 10))
	first := appendDashboardPostgresLane(t, f)
	readHostedCursorFrame(t, stream, "lane", first)
	clock.Add(int64(15 * time.Second))
	f.service.database.postgres.SetBoardCommitNotifier(nil)
	second := appendDashboardPostgresLane(t, f)
	recovery <- time.Now()
	readHostedCursorFrame(t, stream, "lane", second)
	if err := f.provider.RevokeSession(t.Context(), user.identity.Hosted.SessionID); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(6 * time.Second))
	recovery <- time.Now()
	readHostedCursorFrame(t, stream, "reset", second)
	_, err := readSSEFrame(stream)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("idle provider revocation did not close: %v", err)
	}
}

func TestHostedEventsLegacyBoardFiltersAndGap(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, hubdeps.DashboardDelivery{})
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
			if !board.CountsIndependent || board.Work != nil {
				t.Fatalf("board frame replaces independently loaded counts: %+v", board)
			}
			var page tracker.NativeIssuePage
			response := f.request(t, user, http.MethodGet, f.base+"/work-items?include=board"+test.query, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &page)
			total := len(page.Cards)
			if board.Gap != test.gap || len(board.Deltas) != 0 || total != test.total || board.GrantVersion == "" || board.GrantVersion != page.GrantVersion {
				t.Fatalf("legacy frame=%+v total=%d want gap=%t total=%d", board, total, test.gap, test.total)
			}
		})
	}
}

func TestHostedEventsNotifyOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	f, _, _ := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{})
	committed := f.service.database.dashboardCommitted
	repository := f.service.config.TenantRepository
	before := dashboardPostgresHead(t, f)
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.UpdateAccessOrganizationName(t.Context(), "Renamed organization")
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-committed:
		t.Fatal("commit without a delivery notice notified delivery")
	default:
	}
	if head := dashboardPostgresHead(t, f); head != before {
		t.Fatalf("unrelated commit advanced delivery head: got %d, want %d", head, before)
	}
	rollback := errors.New("abort delivery mutation")
	for _, commit := range []bool{false, true} {
		err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			if err := dashboardPostgresLane(t.Context(), w, f.project); err != nil {
				return err
			}
			select {
			case <-committed:
				t.Fatal("transaction notified before commit")
			default:
			}
			if !commit {
				return rollback
			}
			return nil
		})
		if commit && err != nil || !commit && !errors.Is(err, rollback) {
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
		want := before
		if commit {
			want++
		}
		if head := dashboardPostgresHead(t, f); head != want {
			t.Fatalf("commit=%t head=%d want=%d", commit, head, want)
		}
	}
	if head := dashboardPostgresAppend(t, f, "returning-write"); head != before+3 {
		t.Fatalf("typed write returning a row advanced head to %d", head)
	}
	select {
	case <-committed:
	default:
		t.Fatal("typed write returning a row did not notify")
	}
}

func TestHostedEventsLegacyBoardPagesResumeForBothSubscribers(t *testing.T) {
	t.Parallel()
	f, user, d := controlledHostedDelivery(t, hubdeps.DashboardDelivery{PageSize: 1})
	f.seedIssue(t, 1)
	f.seedIssue(t, 2)
	before := hostedDeliveryHead(t, f)
	drainTestNativeBoard(t, f.service)
	head := hostedDeliveryHead(t, f)
	first := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(before, 10))
	second := openHostedTestStream(t, f, user, "?board=true&since="+strconv.FormatInt(before, 10))
	d.changed()
	for d.remaining > 0 {
		d.deliver(t.Context())
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
			if board.Sequence <= previous || len(board.Deltas) > 1 || board.Gap || !board.CountsIndependent || board.Work != nil {
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
}

func TestHostedEventsDeletedEntityVisibility(t *testing.T) {
	t.Parallel()
	for _, entity := range []string{"item", "project"} {
		t.Run(entity, func(t *testing.T) {
			f, user, d := controlledPostgresDelivery(t, hubdeps.DashboardDelivery{})
			item := dashboardPostgresIssue(t, f, 1)
			before := dashboardPostgresHead(t, f)
			query := "?since=" + strconv.FormatInt(before, 10)
			if entity == "item" {
				query += "&work_item=" + string(item)
			}
			stream := openHostedTestStream(t, f, user, query)
			pumpHostedDelivery(t, d)
			if entity == "item" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					return w.DeleteWorkItem(t.Context(), f.project, item)
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := usagePostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE projects SET deleted_at=$1 WHERE organization_id='org_security' AND id=$2", f.service.config.Clock(), f.project); err != nil {
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
			cfg := hubdeps.DashboardDelivery{NewTimer: func(time.Duration) hubdeps.Timer {
				close(ready)
				return hubdeps.Timer{C: tick, Reset: func(time.Duration) {}, Stop: func() {}}
			}}
			s := &Service{config: WithDeps(Config{Logger: discardLogger()}, hubdeps.Deps{DashboardDelivery: &cfg})}
			s.dashboard = newDashboardDelivery(s)
			ctx, cancel := context.WithCancel(t.Context())
			writer := failedDashboardDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{}, 1)}
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx), writer)
			result := make(chan error, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				result <- s.serveDashboardEvents(c, hubscope.NativeScope{Organization: "org_example"}, 0, false, false, "", "")
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
