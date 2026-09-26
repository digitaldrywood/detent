package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
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

	reconnect := f.open(t, f.token, id, 4)
	frame := reconnect.next(t)
	if frame.Event != string(conversation.EventHeartbeat) || frame.Data != `{"seq":4}` {
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
		f.service.conversations.broker.closeAll()
		requireClosed(t, stream.nextEvent(t), "server_shutdown")
	})
}
