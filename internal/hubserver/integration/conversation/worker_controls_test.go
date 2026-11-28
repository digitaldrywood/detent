package conversation_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestConversationWorkerControlsMaximumWait(t *testing.T) {
	t.Parallel()
	type pollKey struct{}
	budgets := make(chan time.Duration, 1)
	timers := make(chan time.Duration, 1)
	fire := make(chan time.Time, 1)
	cfg := conversationTestConfig()
	cfg.Deps().RequestTimeout = func(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
		if ctx.Value(pollKey{}) != nil {
			budgets <- budget
		}
		return context.WithTimeout(ctx, budget)
	}
	cfg.Deps().NewConversationControlTimer = func(wait time.Duration) hubdeps.Timer {
		timers <- wait
		return hubdeps.Timer{C: fire, Stop: func() {}}
	}
	h := hubfixture.Open(t, cfg)
	f := newWorkerConversationProject(t, h, "Idle worker controls")
	for _, tc := range []struct {
		name        string
		callerLimit bool
	}{
		{name: "maximum wait returns an empty page"},
		{name: "caller cancels before the maximum wait", callerLimit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), pollKey{}, true)
			var cancel context.CancelFunc
			if tc.callerLimit {
				ctx, cancel = context.WithTimeout(ctx, 20*time.Second)
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			wait := testenv.WaitContext(t, t.Context())
			response := httptest.NewRecorder()
			done := make(chan struct{})
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				h.Handler().ServeHTTP(response, f.controlsRequest(ctx, 0, 30))
			}()
			select {
			case duration := <-timers:
				if duration != 30*time.Second {
					t.Fatalf("control wait = %s, want 30s", duration)
				}
			case <-done:
				t.Fatalf("poll returned before creating its timer: status %d", response.Code)
			case <-wait.Done():
				t.Fatal("poll did not create its timer")
			}
			if tc.callerLimit {
				select {
				case budget := <-budgets:
					t.Fatalf("caller-supplied deadline was replaced by %s", budget)
				default:
				}
				cancel()
			} else {
				var budget time.Duration
				select {
				case budget = <-budgets:
				case <-wait.Done():
					t.Fatal("poll did not use a server budget")
				}
				if budget <= 30*time.Second {
					t.Fatalf("server budget %s leaves no headroom for the 30s controls wait", budget)
				}
				fire <- time.Now()
			}
			select {
			case <-done:
			case <-wait.Done():
				t.Fatal("poll did not return")
			}
			if tc.callerLimit {
				if response.Code == http.StatusOK {
					t.Fatal("canceled poll returned a successful page")
				}
				return
			}
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page conversations.WorkerControlsResponse
			hubfixture.Decode(t, response, &page)
			if len(page.Controls) != 0 || page.Cursor != 0 {
				t.Fatalf("idle poll returned %+v, want an empty page with the original cursor", page)
			}
		})
	}
}

func TestConversationWorkerControlsLongPoll(t *testing.T) {
	t.Parallel()
	h := openConversationTestHub(t)
	h.Admin.SetMaxOpenConns(2)
	f := newWorkerConversationProject(t, h, "Worker control delivery")
	enqueue := func(t *testing.T, command conversation.Command) conversation.Receipt {
		t.Helper()
		response := f.project.Request(t, http.MethodPost, f.path()+"/commands", command)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var receipt conversation.Receipt
		hubfixture.Decode(t, response, &receipt)
		if receipt.Key != command.Key || receipt.MessageID == "" {
			t.Fatalf("command %s returned receipt %+v", command.Key, receipt)
		}
		return receipt
	}
	snapshot := func(t *testing.T) conversations.ConversationSnapshot {
		t.Helper()
		return readRouteConversation(t, f.project, f.id)
	}
	requireDelivery := func(t *testing.T, id string, status conversation.Delivery) {
		t.Helper()
		for _, message := range snapshot(t).Messages {
			if message.ID == id {
				if message.Delivery != status {
					t.Fatalf("message %s delivery = %s, want %s", id, message.Delivery, status)
				}
				return
			}
		}
		t.Fatalf("message %s is missing", id)
	}
	decodeControls := func(t *testing.T, response *httptest.ResponseRecorder) conversations.WorkerControlsResponse {
		t.Helper()
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var page conversations.WorkerControlsResponse
		hubfixture.Decode(t, response, &page)
		return page
	}
	requireControl := func(t *testing.T, page conversations.WorkerControlsResponse, receipt conversation.Receipt, kind string) {
		t.Helper()
		for _, control := range page.Controls {
			if control.Key == receipt.Key && control.MessageID == receipt.MessageID && control.Kind == kind && page.Cursor == control.Cursor {
				return
			}
		}
		t.Fatalf("page did not deliver %s as %s: %+v", receipt.MessageID, kind, page)
	}
	requireUnchangedCursor := func(t *testing.T, page conversations.WorkerControlsResponse, cursor int64) {
		t.Helper()
		if page.Cursor != cursor {
			t.Fatalf("empty poll advanced the cursor from %d to %d", cursor, page.Cursor)
		}
		for _, control := range page.Controls {
			t.Errorf("empty poll replayed control %s", control.Key)
		}
	}

	first := enqueue(t, conversation.Command{Key: "steer-1", Kind: conversation.CommandMessage, Text: "Also do this"})
	initial := decodeControls(t, f.controls(t, 0, 5))
	requireControl(t, initial, first, "message")
	requireDelivery(t, first.MessageID, conversation.DeliverySending)

	t.Run("acknowledge marks sent once", func(t *testing.T) {
		before := snapshot(t).Cursor
		requireUnchangedCursor(t, decodeControls(t, f.controls(t, initial.Cursor, 0)), initial.Cursor)
		requireDelivery(t, first.MessageID, conversation.DeliverySent)
		if err := h.Repository.ReadTenant(t.Context(), f.project.Organization(), corestore.Serving, func(reader tenantstore.Reader) error {
			command, found, err := reader.Command(t.Context(), f.id, first.Key)
			if err != nil {
				return err
			}
			if !found || command.Receipt.Status != conversation.DeliverySent {
				return errors.New("sent command receipt is missing")
			}
			page, err := reader.ConversationEvents(t.Context(), f.id, before, 100)
			if err != nil {
				return err
			}
			updated := slices.IndexFunc(page.Items, func(event conversationstore.Event) bool { return event.Type == conversation.EventMessageUpdated })
			receipt := slices.IndexFunc(page.Items, func(event conversationstore.Event) bool { return event.Type == conversation.EventCommandReceipt })
			if updated < 0 || receipt <= updated {
				return errors.New("message update must precede the sent receipt")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		after := snapshot(t).Cursor
		requireUnchangedCursor(t, decodeControls(t, f.controls(t, initial.Cursor, 0)), initial.Cursor)
		if got := snapshot(t).Cursor; got != after {
			t.Fatalf("repeated acknowledgement published events: cursor moved from %d to %d", after, got)
		}
	})

	var cursor int64
	t.Run("delivers a committed interrupt to a waiting poll", func(t *testing.T) {
		poll := startWorkerControlPoll(t, f, initial.Cursor)
		second := enqueue(t, conversation.Command{Key: "stop-1", Kind: conversation.CommandInterrupt, Expected: conversation.Expected{AttemptID: f.attempt}})
		page := decodeControls(t, poll(t))
		requireControl(t, page, second, "interrupt")
		cursor = page.Cursor
	})
	if cursor == 0 {
		t.FailNow()
	}
	t.Run("returns no controls for the acknowledged cursor", func(t *testing.T) {
		requireUnchangedCursor(t, decodeControls(t, f.controls(t, cursor, 1)), cursor)
	})
	t.Run("wrong attempt is stale", func(t *testing.T) {
		stale := f
		stale.attempt = "attempt_stale"
		hubfixture.RequireCode(t, stale.controls(t, 0, 0), http.StatusConflict, "stale_execution")
	})
	t.Run("shutdown drains a waiting poll", func(t *testing.T) {
		poll := startWorkerControlPoll(t, f, cursor)
		if err := h.Service.Shutdown(testenv.WaitContext(t, t.Context())); err != nil {
			t.Fatal(err)
		}
		requireUnchangedCursor(t, decodeControls(t, poll(t)), cursor)
	})
}

func startWorkerControlPoll(t *testing.T, f workerConversationProject, after int64) func(*testing.T) *httptest.ResponseRecorder {
	t.Helper()
	admin := f.project.Hub.Admin
	writer, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	var blocker int
	err = writer.QueryRowContext(t.Context(), "SELECT pg_backend_pid() FROM conversations WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.project.Organization(), f.id).Scan(&blocker)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	request := f.controlsRequest(ctx, after, 30)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		f.project.Hub.Handler().ServeHTTP(response, request)
	}()
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-done:
			return nil
		default:
		}
		var waiting bool
		err := admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))", blocker).Scan(&waiting)
		if err != nil {
			return err
		}
		if !waiting {
			return errors.New("control poll has not reached the conversation writer")
		}
		return nil
	})
	select {
	case <-done:
		t.Fatalf("control poll returned before reaching the conversation writer: %s", response.Body.String())
	default:
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	return func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case <-done:
			return response
		case <-testenv.WaitContext(t, t.Context()).Done():
			t.Fatal("control poll did not return")
			return nil
		}
	}
}
