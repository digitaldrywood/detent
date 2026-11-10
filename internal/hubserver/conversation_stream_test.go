package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type sseFrame struct {
	ID    string
	Event string
	Data  string
}

// readSSEFrame reads one frame (terminated by a blank line) from reader.
func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	seen := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return frame, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if seen {
				return frame, nil
			}
			continue
		}
		seen = true
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			frame.ID = value
		case "event":
			frame.Event = value
		case "data":
			frame.Data = value
		}
	}
}

type sseStream struct {
	response *http.Response
	reader   *bufio.Reader
	cancel   context.CancelFunc
}

func (s *sseStream) next(t *testing.T) sseFrame {
	t.Helper()
	frame, err := readSSEFrame(s.reader)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

// nextEvent skips heartbeats and returns the next non-heartbeat frame.
func (s *sseStream) nextEvent(t *testing.T) sseFrame {
	t.Helper()
	for {
		frame := s.next(t)
		if frame.Event != string(conversation.EventHeartbeat) {
			return frame
		}
	}
}

func (s *sseStream) close() {
	s.cancel()
	_ = s.response.Body.Close()
}

// streamFixture serves the hub over a real listener so the SSE body can be
// read incrementally while other requests mutate the conversation.
type streamFixture struct {
	conversationAPIFixture
	server *httptest.Server
}

func newStreamFixture(t *testing.T) streamFixture {
	t.Helper()
	return newStreamFixtureWithAPI(t, newConversationAPIFixture(t, nil))
}

func newStreamFixtureWithAPI(t *testing.T, f conversationAPIFixture) streamFixture {
	t.Helper()
	server := httptest.NewServer(f.service.Handler())
	t.Cleanup(server.Close)
	restoreHeartbeat, restoreAuthorize := conversationStreamHeartbeat, conversationStreamAuthorize
	conversationStreamHeartbeat, conversationStreamAuthorize = 40*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { conversationStreamHeartbeat, conversationStreamAuthorize = restoreHeartbeat, restoreAuthorize })
	return streamFixture{conversationAPIFixture: f, server: server}
}

func (f streamFixture) open(t *testing.T, token, id string, after int64) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.server.URL+f.base+"/conversations/"+id+"/events?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		cancel()
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		cancel()
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		cancel()
		t.Fatalf("cache control = %q", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		cancel()
		t.Fatalf("x-accel-buffering = %q", got)
	}
	stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), cancel: cancel}
	t.Cleanup(stream.close)
	return stream
}

func requireClosed(t *testing.T, frame sseFrame, reason string) {
	t.Helper()
	if frame.Event != string(conversation.EventClosed) || frame.ID != "" {
		t.Fatalf("frame = %#v, want closed", frame)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(frame.Data), &body); err != nil || body.Reason != reason {
		t.Fatalf("closed data = %q, want reason %q", frame.Data, reason)
	}
}

func TestConversationStreamReplaysAndFollows(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	f := newStreamFixture(t)
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	created := f.create(t, f.token, map[string]any{"first_message": map[string]any{"key": "first", "text": "Stream me"}})
	id := created.Conversation.ID
	t.Run("replay stops at authorized head", func(t *testing.T) {
		response := httptest.NewRecorder()
		bounded := newConversationStream(f.service.echo.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response))
		scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
		cursor, err := f.service.replayPGConversationEvents(t.Context(), scope, bounded, id, 0, 1)
		if err != nil || cursor != 1 || strings.Contains(response.Body.String(), string(conversation.EventCommandReceipt)) {
			t.Fatalf("replay exceeded authorized head: cursor=%d error=%v data=%s", cursor, err, response.Body.String())
		}
	})
	stream := f.open(t, f.token, id, 0)
	first := stream.nextEvent(t)
	second := stream.nextEvent(t)
	if first.ID != "1" || first.Event != string(conversation.EventMessageAccepted) ||
		second.ID != "2" || second.Event != string(conversation.EventCommandReceipt) {
		t.Fatalf("frames = %#v, %#v", first, second)
	}
	var message conversationMessageResource
	if err := json.Unmarshal([]byte(first.Data), &message); err != nil || message.Text != "Stream me" || message.ID != created.Receipt.MessageID {
		t.Fatalf("message frame = %q (%v)", first.Data, err)
	}
	heartbeat := stream.next(t)
	if heartbeat.Event != string(conversation.EventHeartbeat) || heartbeat.ID != "" || heartbeat.Data != `{"seq":2}` {
		t.Fatalf("heartbeat = %#v", heartbeat)
	}
	response := f.command(t, f.token, id, conversation.Command{Key: "second", Kind: conversation.CommandMessage, Text: "Follow up"})
	requireNativeStatus(t, response, http.StatusOK)
	third := stream.nextEvent(t)
	fourth := stream.nextEvent(t)
	if third.ID != "3" || third.Event != string(conversation.EventMessageAccepted) || fourth.ID != "4" || fourth.Event != string(conversation.EventCommandReceipt) {
		t.Fatalf("frames = %#v, %#v", third, fourth)
	}
	stream.close()

	if err := f.service.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	f.service = tfConvReopenConversationService(t, f.service)
	f.server = httptest.NewServer(f.service.Handler())
	t.Cleanup(f.server.Close)
	response = f.command(t, f.token, id, conversation.Command{Key: "gap", Kind: conversation.CommandMessage, Text: "During the gap"})
	requireNativeStatus(t, response, http.StatusOK)

	reconnect := f.open(t, f.token, id, 4)
	for _, seq := range []string{"5", "6"} {
		if frame := reconnect.nextEvent(t); frame.ID != seq {
			t.Fatalf("restart replay frame = %#v, want %s", frame, seq)
		}
	}
	frame := reconnect.next(t)
	if frame.Event != string(conversation.EventHeartbeat) || frame.Data != `{"seq":6}` {
		t.Fatalf("reconnect frame = %#v, want heartbeat without duplicates", frame)
	}
	reconnect.close()

	partial := f.open(t, f.token, id, 2)
	if frame := partial.nextEvent(t); frame.ID != "3" {
		t.Fatalf("partial replay frame = %#v", frame)
	}
	partial.close()

	ahead := f.open(t, f.token, id, 99)
	requireClosed(t, ahead.next(t), "cursor_expired")
	if _, err := readSSEFrame(ahead.reader); !errors.Is(err, io.EOF) {
		t.Fatalf("stream after closed: err = %v, want EOF", err)
	}
}

func TestConversationStreamRejectsInvalidCursorAndHiddenConversation(t *testing.T) {
	f := newStreamFixture(t)
	id := f.create(t, f.token, map[string]any{"title": "Hidden"}).Conversation.ID
	tests := []struct {
		name   string
		token  string
		path   string
		status int
	}{
		{name: "invalid after", token: f.token, path: f.base + "/conversations/" + id + "/events?after=x", status: http.StatusUnprocessableEntity},
		{name: "private is opaque", token: f.other, path: f.base + "/conversations/" + id + "/events?after=0", status: http.StatusNotFound},
		{name: "unknown conversation", token: f.token, path: f.base + "/conversations/conv_nope/events", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, tt.path, tt.token, nil)
			requireNativeStatus(t, response, tt.status)
		})
	}
	t.Run("compaction does no archive IO under a lock and refuses changed source", checkCompactionPublicationSource)
	t.Run("settle tick drives typed compaction", checkCompactionSettleTick)
	t.Run("replay does not silently cross concurrent compaction", checkCompactionReplaySnapshot)
	t.Run("compaction projection and watermarks survive restart", checkCompactionRestart)
	t.Run("restart invalidates terminal message eligibility", checkCompactionRestartEligibility)
	t.Run("append preserves progress past retained prefix", checkCompactionAppendProgress)
	t.Run("replacement append invalidates only its predecessor", checkCompactionPredecessorInvalidation)
	t.Run("pending lookup is tenant and conversation bounded", checkCompactionPendingLookup)
	t.Run("bounded compaction is fair across conversations", func(t *testing.T) {
		f := newStreamFixture(t)
		now := time.Now().UTC().Truncate(time.Microsecond)
		old := now.Add(-31 * 24 * time.Hour)
		firstID := f.create(t, f.token, map[string]any{"title": "Example first history"}).Conversation.ID
		secondID := f.create(t, f.token, map[string]any{"title": "Example second history"}).Conversation.ID
		if firstID > secondID {
			firstID, secondID = secondID, firstID
		}
		first := seedStreamCompactionConversation(t, f, old.Add(-time.Hour), 1200, firstID)
		second := seedStreamCompactionConversation(t, f, old, 1, secondID)
		removed, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now)
		if err != nil {
			t.Fatal(err)
		}
		if removed != conversationstore.CompactionBatch-1 {
			t.Fatalf("first tick removed=%d, want %d after retaining the snapshot", removed, conversationstore.CompactionBatch-1)
		}
		firstPage := readStreamCompactionConversation(t, f, first.ID)
		if firstPage.Compaction.ScannedSequence != conversationstore.CompactionBatch || firstPage.Compaction.ThroughSequence != conversationstore.CompactionBatch {
			t.Fatalf("first bounded page = %+v", firstPage.Compaction)
		}
		if got := readStreamCompactionConversation(t, f, second.ID); got.Compaction.ThroughSequence != 0 {
			t.Fatalf("first tick exceeded its event budget: %+v", got.Compaction)
		}
		if _, err := f.service.conversations.compactEventsPG(t.Context(), f.project.OrganizationID, now); err != nil {
			t.Fatal(err)
		}
		if got := readStreamCompactionConversation(t, f, second.ID); got.Compaction.ThroughSequence != second.EventSequence {
			t.Fatalf("second conversation starved behind a large history: %+v", got.Compaction)
		}
		if got := readStreamCompactionConversation(t, f, first.ID); got.Compaction != firstPage.Compaction {
			t.Fatalf("second tick repeated the first conversation ahead of its peer: %+v", got.Compaction)
		}
	})
	t.Run("compacted cursor requests a snapshot", func(t *testing.T) {
		old := time.Now().UTC().Add(-31 * 24 * time.Hour)
		plan, err := f.service.conversations.newPublicationPlan(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]string{"text": "transient"}
		if err := plan.event(t.Context(), body); err != nil {
			t.Fatal(err)
		}
		ctx := plan.context(t.Context())
		if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
			if err := appendPGConversationEvent(ctx, writer, f.project.OrganizationID, id, conversation.EventMessageDelta, body, old); err != nil {
				return err
			}
			record, err := writer.Conversation(ctx, f.project.ID, id)
			if err != nil {
				return err
			}
			record.Status, record.SettledAt, record.UpdatedAt = conversation.StatusSettled, &old, old
			_, err = writer.UpdateConversation(ctx, record, record.Revision)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.maintainNativeRetention(t.Context(), time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/conversations/"+id, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var snapshot conversationSnapshot
		decodeHubResponse(t, response, &snapshot)
		if snapshot.Compaction == nil || snapshot.Compaction.RemovedEvents != 1 || snapshot.Cursor < 1 {
			t.Fatalf("snapshot = %+v", snapshot)
		}
		stream := f.open(t, f.token, id, 0)
		defer stream.close()
		frame := stream.nextEvent(t)
		if frame.Event != string(conversation.EventClosed) || !strings.Contains(frame.Data, conversationClosedCursorExpired) {
			t.Fatalf("stale stream = %+v", frame)
		}
	})
}

func TestConversationStreamClosesOnRevocationAndShutdown(t *testing.T) {
	t.Run("access revoked", func(t *testing.T) {
		f := newStreamFixture(t)
		id := f.create(t, f.token, map[string]any{"title": "Revoked"}).Conversation.ID
		stream := f.open(t, f.token, id, 0)
		if frame := stream.next(t); frame.Event != string(conversation.EventHeartbeat) {
			t.Fatalf("frame = %#v", frame)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(writer tenantstore.Writer) error {
			return writer.RevokeAccessCredential(t.Context(), f.ownerID, f.service.config.now())
		}); err != nil {
			t.Fatal(err)
		}
		requireClosed(t, stream.nextEvent(t), "access_revoked")
	})
	t.Run("server shutdown", func(t *testing.T) {
		f := newStreamFixture(t)
		id := f.create(t, f.token, map[string]any{"title": "Shutdown"}).Conversation.ID
		stream := f.open(t, f.token, id, 0)
		if frame := stream.next(t); frame.Event != string(conversation.EventHeartbeat) {
			t.Fatalf("frame = %#v", frame)
		}
		if err := f.service.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		requireClosed(t, stream.nextEvent(t), "server_shutdown")
	})
}

func TestShutdownClosesConversationStreamBeforeOuterServerDrain(t *testing.T) {
	f := newStreamFixture(t)
	created := f.create(t, f.token, map[string]any{"first_message": map[string]any{"key": "first", "text": "Hold the stream open"}})
	stream := f.open(t, f.token, created.Conversation.ID, 2)
	if heartbeat := stream.next(t); heartbeat.Event != string(conversation.EventHeartbeat) {
		t.Fatalf("active stream frame = %#v, want heartbeat", heartbeat)
	}
	closed := make(chan sseFrame, 1)
	go func() {
		for {
			frame, err := readSSEFrame(stream.reader)
			if err != nil {
				closed <- sseFrame{}
				return
			}
			if frame.Event == string(conversation.EventClosed) {
				closed <- frame
				return
			}
		}
	}()
	serviceShutdownDone := make(chan struct{})
	shutdownErr := shutdownServiceAndServer(t.Context(), func(ctx context.Context) error {
		defer close(serviceShutdownDone)
		return f.service.Shutdown(ctx)
	}, func(ctx context.Context) error {
		select {
		case <-serviceShutdownDone:
		default:
			return errors.New("outer server drain started before service shutdown")
		}
		select {
		case frame := <-closed:
			if frame.Event != string(conversation.EventClosed) || !strings.Contains(frame.Data, conversationClosedShutdown) {
				return errors.New("conversation stream ended without shutdown event")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return f.server.Config.Shutdown(ctx)
	})
	if shutdownErr != nil {
		t.Fatalf("shutdown active conversation stream: %v", shutdownErr)
	}
}

type streamMembershipFailureProvider struct {
	auth.HostedProvider
	failure atomic.Pointer[streamMembershipFailure]
}

type streamMembershipFailure struct {
	err error
}

type streamHeartbeatRecorder struct {
	*httptest.ResponseRecorder
	heartbeat chan struct{}
}

func (r *streamHeartbeatRecorder) Write(data []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(data)
	if r.heartbeat != nil && strings.Contains(r.Body.String(), "event: heartbeat\n") {
		close(r.heartbeat)
		r.heartbeat = nil
	}
	return n, err
}

func (r *streamHeartbeatRecorder) WriteString(data string) (int, error) {
	return r.Write([]byte(data))
}

func (p *streamMembershipFailureProvider) Memberships(ctx context.Context, user, organization string) ([]auth.Membership, error) {
	if failure := p.failure.Load(); failure != nil {
		return nil, failure.err
	}
	return p.HostedProvider.Memberships(ctx, user, organization)
}

func (p *streamMembershipFailureProvider) fail(err error) {
	p.failure.Store(&streamMembershipFailure{err: err})
}

func TestConversationStreamReauthorizationCloseReason(t *testing.T) {
	heartbeat := conversationStreamHeartbeat
	conversationStreamHeartbeat = 40 * time.Millisecond
	t.Cleanup(func() { conversationStreamHeartbeat = heartbeat })
	for _, tt := range []struct {
		name             string
		account          string
		mutate           func(*testing.T, *browserHostedFixture, nativeScope)
		refresh          bool
		shared           bool
		wantErr          error
		wantError        string
		credentialStatus int
		want             string
	}{
		{
			name: "screen read audience changed", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversations SET read_audience='different-screen-access' WHERE organization_id=$1 AND id=$2", scope.organization, f.conversation); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "expired session cookie", account: "owner", refresh: true, want: conversationClosedServerError, wantErr: auth.ErrInvalidSession,
		},
		{
			name: "expired provider session", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrInvalidSession,
			mutate: func(_ *testing.T, f *browserHostedFixture, scope nativeScope) {
				f.provider.mu.Lock()
				defer f.provider.mu.Unlock()
				identity := f.provider.sessions[scope.credential.Hosted.SessionID]
				identity.ExpiresAt = time.Now().Add(-time.Second)
				f.provider.sessions[identity.SessionID] = identity
			},
		},
		{
			name: "membership removed", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(_ *testing.T, f *browserHostedFixture, scope nativeScope) {
				f.provider.mu.Lock()
				defer f.provider.mu.Unlock()
				delete(f.provider.members, scope.credential.HostedMembership)
			},
		},
		{
			name: "local membership inactive", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE hosted_members SET active=false WHERE organization_id=$1", scope.organization); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shared membership inactive", account: "owner", shared: true, want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE hosted_members SET active=false WHERE organization_id=$1", scope.organization); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member token revoked", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=$1 WHERE organization_id=$2", f.service.config.now(), scope.organization); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tenant missing", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM hosted_tenant WHERE organization_id=$1", scope.organization); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong provider organization", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE hosted_tenant SET provider_id='org_other' WHERE organization_id=$1", scope.organization); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tenant read failure", account: "owner", want: conversationClosedServerError, wantError: "SQLSTATE 42P01",
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "ALTER TABLE hosted_tenant RENAME TO unavailable_tenant"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member read failure", account: "owner", want: conversationClosedServerError, wantError: "SQLSTATE 42P01",
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "ALTER TABLE hosted_members RENAME TO unavailable_members"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shared member read failure", account: "owner", shared: true, want: conversationClosedServerError, wantError: "SQLSTATE 42P01",
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "ALTER TABLE hosted_members RENAME TO unavailable_members"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "grant read failure", account: "owner", want: conversationClosedServerError, wantError: "SQLSTATE 42P01",
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "ALTER TABLE hosted_project_grants RENAME TO unavailable_grants"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "provider timeout", account: "owner", want: conversationClosedServerError, wantErr: context.DeadlineExceeded, wantError: "context deadline exceeded",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider.(*streamMembershipFailureProvider).fail(context.DeadlineExceeded)
			},
		},
		{
			name: "provider rate limit", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "status 429", credentialStatus: http.StatusServiceUnavailable,
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider.(*streamMembershipFailureProvider).fail(&auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable, Status: http.StatusTooManyRequests})
			},
		},
		{
			name: "provider unavailable", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "status 503",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider.(*streamMembershipFailureProvider).fail(&auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable, Status: http.StatusServiceUnavailable})
			},
		},
		{
			name: "provider lookup sentinel", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "tenant authority storage is unavailable",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider.(*streamMembershipFailureProvider).fail(auth.ErrHostedIdentity)
			},
		},
		{
			name: "project grant removed", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2 AND project_id=$3", scope.organization, scope.credential.Hosted.Subject, scope.project); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conversation made private", account: "viewer", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "UPDATE conversations SET visibility='private' WHERE organization_id=$1 AND id=$2", scope.organization, f.conversation); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, browserPreviewConfig)
			f.seedConversation(t)
			f.service.config.Hosted.Provider = &streamMembershipFailureProvider{HostedProvider: f.provider}
			request := httptest.NewRequest(http.MethodGet, f.server.URL+browserHostedOrganizationBase+"/projects/"+f.project+"/conversations/"+f.conversation+"/events", nil)
			request.AddCookie(f.cookies[tt.account])
			c := echo.New().NewContext(request, httptest.NewRecorder())
			credential, _, err := f.service.hostedCredential(c.Request().Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project), credential: credential}
			if tt.shared {
				var session accessstore.Session
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.organization, corestore.Serving, func(reader tenantstore.Reader) error {
					var err error
					session, err = reader.AccessSession(t.Context(), credential.SessionHash)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.service.config.Hosted.SharedEntry = &HostedSharedEntry{Generation: 1}
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{
					Audience: string(scope.organization), Generation: 1,
					Kind: cloudassert.KindBrowser, Subject: session.Subject, Email: session.Email,
					ProviderOrganization: session.ProviderOrganizationID, ProviderSession: session.SessionID,
					SessionCreatedAt: session.IdentityCreatedAt, SessionExpiresAt: session.IdentityExpiresAt,
					Role: credential.HostedRole, AccessExpiresAt: session.ExpiresAt, Binding: session.Hash,
					CSRF: cloudassert.CSRFToken("stream", string(scope.organization)),
				})
			}
			if _, err := f.service.reauthorizeConversationStream(c, &scope, f.conversation); err != nil {
				t.Fatalf("initial authorization: %v", err)
			}
			verifyStream := tt.name != "conversation made private" && tt.name != "screen read audience changed"
			var streamDone chan struct{}
			var streamErr error
			var response *httptest.ResponseRecorder
			var sink *conversationLogSink
			if verifyStream {
				sink = &conversationLogSink{}
				f.service.conversations.logger = slog.New(slog.NewJSONHandler(sink, nil)).With("component", "conversation")
				response = httptest.NewRecorder()
				ready := make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())
				streamContext := f.service.echo.NewContext(c.Request().Clone(ctx), &streamHeartbeatRecorder{ResponseRecorder: response, heartbeat: ready})
				streamContext.Set("native_scope", scope)
				if claims, ok := hostedSharedClaims(c); ok {
					streamContext.Set(hostedSharedClaimsKey, claims)
				}
				streamContext.SetParamNames("conversation")
				streamContext.SetParamValues(f.conversation)
				streamDone = make(chan struct{})
				go func() {
					defer close(streamDone)
					streamErr = f.service.streamConversationEvents(streamContext)
				}()
				defer func() { cancel(); <-streamDone }()
				select {
				case <-ready:
				case <-streamDone:
					t.Fatalf("stream ended before heartbeat: %v", streamErr)
				case <-testenv.WaitContext(t, ctx).Done():
					t.Fatal("stream did not emit its first heartbeat")
				}
			}
			refreshHandler := f.sessionRefreshHandler(t, f.service.Handler())
			if tt.refresh {
				expire := httptest.NewRequest(http.MethodPost, f.server.URL+"/__preview/session/expire", nil)
				expire.AddCookie(f.cookies[tt.account])
				response := httptest.NewRecorder()
				refreshHandler.ServeHTTP(response, expire)
				if response.Code != http.StatusNoContent {
					t.Fatalf("expire session: %d %s", response.Code, response.Body)
				}
			} else {
				tt.mutate(t, f, scope)
			}
			if tt.credentialStatus != 0 {
				if _, status, err := f.service.hostedCredential(c.Request().Context(), c); err == nil || status != tt.credentialStatus {
					t.Fatalf("credential status = %d (%v), want %d", status, err, tt.credentialStatus)
				}
			}
			_, err = f.service.reauthorizeConversationStream(c, &scope, f.conversation)
			if err == nil {
				t.Fatal("reauthorization succeeded after access removal or authorization failure")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("reauthorization error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantError != "" && !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("reauthorization error = %v, want %q", err, tt.wantError)
			}
			for _, failure := range []error{err, fmt.Errorf("reauthorize: %w", err)} {
				if got := conversationStreamCloseReason(failure); got != tt.want {
					t.Fatalf("close reason for %v = %q, want %q", failure, got, tt.want)
				}
			}
			if verifyStream {
				f.service.conversations.broker.notify(f.conversation)
				select {
				case <-streamDone:
				case <-testenv.WaitContext(t, t.Context()).Done():
					t.Fatal("stream did not close after reauthorization failure")
				}
				if streamErr != nil {
					t.Fatal(streamErr)
				}
				reader := bufio.NewReader(response.Body)
				var frame sseFrame
				for frame.Event != string(conversation.EventClosed) {
					var frameErr error
					frame, frameErr = readSSEFrame(reader)
					if frameErr != nil {
						t.Fatal(frameErr)
					}
				}
				requireClosed(t, frame, tt.want)
				requireConversationLogFields(t, sink.only(t, "conversation.stream_reauthorize_failed"), map[string]any{
					"conversation_id": f.conversation, "reason": tt.want, "error": err.Error(),
				})
			}
			if tt.refresh {
				reconnect := httptest.NewRequest(http.MethodGet, f.server.URL+browserHostedOrganizationBase+"/projects/"+f.project+"/conversations/"+f.conversation, nil)
				reconnect.AddCookie(f.cookies[tt.account])
				response := httptest.NewRecorder()
				refreshHandler.ServeHTTP(response, reconnect)
				if response.Code != http.StatusOK {
					t.Fatalf("reconnect: %d %s", response.Code, response.Body)
				}
				fresh := response.Result().Cookies()
				if len(fresh) != 1 || fresh[0].Name != hostedCookie || fresh[0].Value == f.cookies[tt.account].Value {
					t.Fatalf("reconnect did not refresh the session cookie: %d cookies", len(fresh))
				}
				c = echo.New().NewContext(reconnect, httptest.NewRecorder())
				if _, err := f.service.reauthorizeConversationStream(c, &scope, f.conversation); err != nil {
					t.Fatalf("reauthorization after reconnect: %v", err)
				}
			}
		})
	}
}
