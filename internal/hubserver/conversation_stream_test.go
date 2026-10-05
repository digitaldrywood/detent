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
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/conversation"
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
	f := newConversationAPIFixture(t, nil)
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
	f := newStreamFixture(t)
	created := f.create(t, f.token, map[string]any{"first_message": map[string]any{"key": "first", "text": "Stream me"}})
	id := created.Conversation.ID
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
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	restarted, err := Open(t.Context(), f.service.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	f.service = restarted
	f.server = httptest.NewServer(restarted.Handler())
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
}

func TestConversationStreamClosesOnRevocationAndShutdown(t *testing.T) {
	f := newStreamFixture(t)
	t.Run("access revoked", func(t *testing.T) {
		id := f.create(t, f.token, map[string]any{"title": "Revoked"}).Conversation.ID
		stream := f.open(t, f.token, id, 0)
		if frame := stream.next(t); frame.Event != string(conversation.EventHeartbeat) {
			t.Fatalf("frame = %#v", frame)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = ? WHERE id = ?", testTimestamp, f.ownerID); err != nil {
			t.Fatal(err)
		}
		requireClosed(t, stream.nextEvent(t), "access_revoked")
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = NULL WHERE id = ?", f.ownerID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("server shutdown", func(t *testing.T) {
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

type streamMembershipFailureProvider struct {
	auth.HostedProvider
	err error
}

func (p streamMembershipFailureProvider) Memberships(context.Context, string, string) ([]auth.Membership, error) {
	return nil, p.err
}

func TestConversationStreamReauthorizationCloseReason(t *testing.T) {
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
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET active = 0"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shared membership inactive", account: "owner", shared: true, want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET active = 0"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member token revoked", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at = '2026-10-02T00:00:00Z'"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tenant missing", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_tenant RENAME TO original_tenant; CREATE TABLE hosted_tenant (singleton INTEGER, provider_id TEXT)"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong provider organization", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_tenant RENAME TO original_tenant; CREATE TABLE hosted_tenant AS SELECT singleton, 'org_other' AS provider_id FROM original_tenant"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "tenant read failure", account: "owner", want: conversationClosedServerError, wantError: "no such table: hosted_tenant",
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_tenant RENAME TO unavailable_tenant"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "member read failure", account: "owner", want: conversationClosedServerError, wantError: "no such table: hosted_members",
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_members RENAME TO unavailable_members"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "shared member read failure", account: "owner", shared: true, want: conversationClosedServerError, wantError: "no such table: hosted_members",
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_members RENAME TO unavailable_members"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "grant read failure", account: "owner", want: conversationClosedServerError, wantError: "no such table: hosted_project_grants",
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE hosted_project_grants RENAME TO unavailable_grants"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "provider timeout", account: "owner", want: conversationClosedServerError, wantErr: context.DeadlineExceeded, wantError: "context deadline exceeded",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider = streamMembershipFailureProvider{HostedProvider: f.provider, err: context.DeadlineExceeded}
			},
		},
		{
			name: "provider rate limit", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "status 429", credentialStatus: http.StatusInternalServerError,
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider = streamMembershipFailureProvider{HostedProvider: f.provider, err: &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable, Status: http.StatusTooManyRequests}}
			},
		},
		{
			name: "provider unavailable", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "status 503",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider = streamMembershipFailureProvider{HostedProvider: f.provider, err: &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable, Status: http.StatusServiceUnavailable}}
			},
		},
		{
			name: "provider lookup sentinel", account: "owner", want: conversationClosedServerError, wantErr: auth.ErrHostedIdentity, wantError: "lookup hosted memberships",
			mutate: func(_ *testing.T, f *browserHostedFixture, _ nativeScope) {
				f.service.config.Hosted.Provider = streamMembershipFailureProvider{HostedProvider: f.provider, err: auth.ErrHostedIdentity}
			},
		},
		{
			name: "project grant removed", account: "owner", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, scope nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id = ? AND project_id = ?", scope.credential.Hosted.Subject, scope.project); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conversation made private", account: "viewer", want: conversationClosedAccessRevoked,
			mutate: func(t *testing.T, f *browserHostedFixture, _ nativeScope) {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET visibility = 'private' WHERE id = ?", f.conversation); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, browserPreviewConfig)
			f.seedConversation(t)
			request := httptest.NewRequest(http.MethodGet, f.server.URL+browserHostedOrganizationBase+"/projects/"+f.project+"/conversations/"+f.conversation+"/events", nil)
			request.AddCookie(f.cookies[tt.account])
			c := echo.New().NewContext(request, httptest.NewRecorder())
			credential, _, err := f.service.hostedCredential(c.Request().Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project), credential: credential}
			if tt.shared {
				identity := credential.Hosted
				f.service.config.Hosted.SharedEntry = &HostedSharedEntry{}
				c.Set(hostedSharedClaimsKey, cloudassert.Claims{
					Kind: cloudassert.KindBrowser, Subject: identity.Subject, Email: "owner@example.test",
					ProviderOrganization: identity.OrganizationID, ProviderSession: identity.SessionID,
					SessionCreatedAt: identity.CreatedAt, SessionExpiresAt: identity.ExpiresAt,
					Role: credential.HostedRole, AccessExpiresAt: identity.ExpiresAt, Binding: credential.SessionHash,
				})
			}
			if _, err := f.service.reauthorizeConversationStream(c, &scope, f.conversation); err != nil {
				t.Fatalf("initial authorization: %v", err)
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
			if tt.name != "conversation made private" {
				sink := &conversationLogSink{}
				f.service.conversations.logger = slog.New(slog.NewJSONHandler(sink, nil)).With("component", "conversation")
				response := httptest.NewRecorder()
				c.Response().Writer = response
				c.Set("native_scope", scope)
				c.SetParamNames("conversation")
				c.SetParamValues(f.conversation)
				if streamErr := f.service.streamConversationEvents(c); streamErr != nil {
					t.Fatal(streamErr)
				}
				frame, frameErr := readSSEFrame(bufio.NewReader(response.Body))
				if frameErr != nil {
					t.Fatal(frameErr)
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
