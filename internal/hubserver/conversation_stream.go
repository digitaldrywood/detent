package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
)

// Stream timing. Variables rather than constants so tests can shorten them.
var errConversationCursorExpired = errors.New("conversation cursor expired")

// Reasons carried by the closed frame.
const (
	conversationClosedCursorExpired = "cursor_expired"
	conversationClosedAccessRevoked = "access_revoked"
	conversationClosedShutdown      = "server_shutdown"
	// conversationClosedServerError reports that the stream could not be
	// re-authorized because the hub failed, not because access changed. The
	// client may reconnect; access_revoked means it must not.
	conversationClosedServerError = "server_error"
)

// conversationStream writes server-sent event frames for one subscriber.
type conversationStream struct {
	response          *echo.Response
	authorize         func() error
	now               func() time.Time
	nextAuthorization time.Time
	authorizationErr  error
}

func newConversationStream(c echo.Context) *conversationStream {
	response := c.Response()
	header := response.Header()
	header.Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	response.Flush()
	return &conversationStream{response: response, now: time.Now}
}

// event writes a committed event with its sequence as the frame id.
func (s *conversationStream) event(event conversation.Event) error {
	if s.authorize != nil {
		if s.authorizationErr != nil {
			return s.authorizationErr
		}
		now := s.now()
		if !now.Before(s.nextAuthorization) {
			s.nextAuthorization = now.Add(conversationStreamAuthorize)
			if err := s.authorize(); err != nil {
				s.authorizationErr = err
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(s.response, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, event.Data); err != nil {
		return err
	}
	s.response.Flush()
	return nil
}

// heartbeat writes the keep-alive frame carrying the subscriber's cursor.
func (s *conversationStream) heartbeat(cursor int64) error {
	if _, err := fmt.Fprintf(s.response, "event: %s\ndata: {\"seq\":%d}\n\n", conversation.EventHeartbeat, cursor); err != nil {
		return err
	}
	s.response.Flush()
	return nil
}

// closed writes the terminal frame; the handler returns afterwards.
func (s *conversationStream) closed(reason string) error {
	data, err := json.Marshal(map[string]string{"reason": reason})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.response, "event: %s\ndata: %s\n\n", conversation.EventClosed, data); err != nil {
		return err
	}
	s.response.Flush()
	return nil
}
