package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
)

// Stream timing. Variables rather than constants so tests can shorten them.
var (
	conversationStreamHeartbeat = conversationHeartbeatInterval
	conversationStreamAuthorize = conversationAuthorizeInterval
)

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
	response *echo.Response
}

func newConversationStream(c echo.Context) *conversationStream {
	response := c.Response()
	header := response.Header()
	header.Set(echo.HeaderContentType, "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	response.WriteHeader(http.StatusOK)
	response.Flush()
	return &conversationStream{response: response}
}

// event writes a committed event with its sequence as the frame id.
func (s *conversationStream) event(event conversation.Event) error {
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

// replay writes every committed event after cursor in pages and returns the
// new cursor.
func (s *conversationStream) replay(ctx context.Context, store *conversationStore, conversationID string, cursor int64) (int64, error) {
	for {
		events, err := store.listEvents(ctx, store.db, conversationID, cursor, conversationEventPage)
		if err != nil {
			return cursor, err
		}
		for _, event := range events {
			if err := s.event(event); err != nil {
				return cursor, err
			}
			cursor = event.Seq
		}
		if len(events) < conversationEventPage {
			return cursor, nil
		}
	}
}

// streamConversationEvents implements GET /conversations/:conversation/events.
// The durable event log is the source: subscribers are only woken, then read
// from their cursor, so a slow client never loses or reorders events. The
// stream re-authorizes on every wake and on a timer and closes on loss.
func (s *Service) streamConversationEvents(c echo.Context) error {
	scope := nativeRequestScope(c)
	after, err := conversationQueryInt(c, "after")
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	service := s.conversations
	ctx := c.Request().Context()
	record, err := service.loadConversation(ctx, s.database.db, scope, c.Param("conversation"))
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	stream := newConversationStream(c)
	if after > record.EventSeq {
		// Every event is retained in this milestone, so only a cursor ahead
		// of the head is outside the replayable window.
		return stream.closed(conversationClosedCursorExpired)
	}
	subscription, cancel := service.broker.subscribe(record.ID)
	defer cancel()
	heartbeat := time.NewTicker(conversationStreamHeartbeat)
	defer heartbeat.Stop()
	authorize := time.NewTicker(conversationStreamAuthorize)
	defer authorize.Stop()
	cursor := after
	for {
		// The re-read proves the stream may continue: a settled conversation
		// keeps streaming, only a revoked audience closes it.
		_, err := s.reauthorizeConversationStream(c, &scope, record.ID)
		if err != nil {
			reason := conversationStreamCloseReason(err)
			if reason == conversationClosedServerError {
				service.logger.Warn("conversation.stream_reauthorize_failed", "conversation_id", record.ID, "error", err)
			}
			return stream.closed(reason)
		}
		if cursor, err = stream.replay(ctx, service.store, record.ID, cursor); err != nil {
			return nil
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-subscription.closed:
				return stream.closed(conversationClosedShutdown)
			case <-heartbeat.C:
				if err := stream.heartbeat(cursor); err != nil {
					return nil
				}
			case <-subscription.wake:
				break wait
			case <-authorize.C:
				break wait
			}
		}
	}
}

// reauthorizeConversationStream re-runs the request authorization from live
// state: the hosted session, membership and grant for sessions, token
// revocation and grants for API tokens, then the conversation read rule.
// It updates scope with the refreshed credential and returns the current
// record.
func (s *Service) reauthorizeConversationStream(c echo.Context, scope *nativeScope, conversationID string) (conversationRecord, error) {
	ctx := c.Request().Context()
	if scope.credential.Hosted != nil {
		credential, _, err := s.hostedCredential(c)
		if err != nil {
			return conversationRecord{}, err
		}
		scope.credential = credential
		if err := s.requireHostedProject(ctx, s.database.db, *scope, false); err != nil {
			return conversationRecord{}, err
		}
	} else {
		err := s.conversations.transact(ctx, func(tx *sql.Tx, now time.Time) error {
			return requireCredentialAuthority(ctx, tx, scope.credential, now)
		})
		if err != nil {
			return conversationRecord{}, err
		}
	}
	if err := s.database.authorizeNativeProject(ctx, *scope); err != nil {
		return conversationRecord{}, err
	}
	return s.conversations.loadConversation(ctx, s.database.db, *scope, conversationID)
}

// conversationStreamCloseReason tells a denial apart from a failure. A
// denial is a decision the client must respect; a failure is transient and
// the client may reconnect.
func conversationStreamCloseReason(err error) string {
	var native *nativeError
	switch {
	case errors.As(err, &native):
		return conversationClosedAccessRevoked
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, auth.ErrInvalidSession), errors.Is(err, auth.ErrHostedIdentity):
		return conversationClosedAccessRevoked
	default:
		return conversationClosedServerError
	}
}
