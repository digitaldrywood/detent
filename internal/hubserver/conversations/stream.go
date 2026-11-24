package conversations

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
)

// Stream timing. Variables rather than constants so tests can shorten them.
var ErrConversationCursorExpired = errors.New("conversation cursor expired")

// Reasons carried by the closed frame.
const (
	ConversationClosedCursorExpired = "cursor_expired"
	ConversationClosedAccessRevoked = "access_revoked"
	ConversationClosedShutdown      = "server_shutdown"
	// conversationClosedServerError reports that the stream could not be
	// re-authorized because the hub failed, not because access changed. The
	// client may reconnect; access_revoked means it must not.
	ConversationClosedServerError = "server_error"
)

// conversationStream writes server-sent event frames for one subscriber.
type ConversationStream struct {
	Response *echo.Response
}

func NewConversationStream(c echo.Context) *ConversationStream {
	response := c.Response()
	header := response.Header()
	header.Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	response.Flush()
	return &ConversationStream{Response: response}
}

// event writes a committed event with its sequence as the frame id.
func (s *ConversationStream) Event(event conversation.Event) error {
	if _, err := fmt.Fprintf(s.Response, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, event.Data); err != nil {
		return err
	}
	s.Response.Flush()
	return nil
}

// heartbeat writes the keep-alive frame carrying the subscriber's cursor.
func (s *ConversationStream) Heartbeat(cursor int64) error {
	if _, err := fmt.Fprintf(s.Response, "event: %s\ndata: {\"seq\":%d}\n\n", conversation.EventHeartbeat, cursor); err != nil {
		return err
	}
	s.Response.Flush()
	return nil
}

// closed writes the terminal frame; the handler returns afterwards.
func (s *ConversationStream) Closed(reason string) error {
	data, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.Response, "event: %s\ndata: %s\n\n", conversation.EventClosed, data); err != nil {
		return err
	}
	s.Response.Flush()
	return nil
}
