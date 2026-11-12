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
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

// Stream timing. Variables rather than constants so tests can shorten them.
var (
	conversationStreamHeartbeat  = conversationHeartbeatInterval
	conversationStreamAuthorize  = conversationAuthorizeInterval
	errConversationCursorExpired = errors.New("conversation cursor expired")
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
	var record conversationRecord

	record, err = s.readPGConversation(ctx, scope, c.Param("conversation"))

	if err != nil {
		return s.nativeAPIError(c, err)
	}
	stream := newConversationStream(c)
	stream.now = s.config.now
	if scope.credential.EntryKey != nil {
		stream.authorize = func() error {
			_, err := s.reauthorizeConversationStream(c, &scope, record.ID)
			return err
		}
	}
	if after > record.EventSeq {
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
		stream.nextAuthorization = stream.now().Add(conversationStreamAuthorize)
		authorized, err := s.reauthorizeConversationStream(c, &scope, record.ID)
		if err != nil {
			reason := conversationStreamCloseReason(err)
			service.logger.Warn("conversation.stream_reauthorize_failed", "conversation_id", record.ID, "reason", reason, "error", err)
			return stream.closed(reason)
		}

		cursor, err = s.replayPGConversationEvents(ctx, scope, stream, record.ID, cursor, authorized.EventSeq)

		if err != nil {
			if errors.Is(err, errConversationCursorExpired) {
				return stream.closed(conversationClosedCursorExpired)
			}
			return stream.closed(conversationStreamCloseReason(err))
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
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	if scope.credential.EntryKey != nil {
		claims, ok := hostedSharedClaims(c)
		if !ok || claims.Key == nil {
			return conversationRecord{}, auth.ErrHostedIdentity
		}
		encoded, err := json.Marshal(entryauthority.Origin{KeyID: claims.Key.ID, CredentialBinding: claims.Key.CredentialBinding, Subject: claims.Subject, Issuer: claims.Issuer, Generation: claims.Generation})
		if err != nil {
			return conversationRecord{}, err
		}
		credential, err := s.currentEntryOriginCredential(readCtx, string(encoded))
		if err != nil {
			return conversationRecord{}, err
		}
		scope.credential = credential
	} else if scope.credential.Hosted != nil {
		credential, _, err := s.authenticateAPIRequestContext(readCtx, c)
		if err != nil {
			return conversationRecord{}, err
		}
		scope.credential = credential
	}

	if err := s.authorizeProjectRecord(readCtx, *scope, false); err != nil {
		return conversationRecord{}, err
	}
	if _, err := s.readProjectRecord(readCtx, *scope); err != nil {
		return conversationRecord{}, err
	}

	return s.readPGConversation(readCtx, *scope, conversationID)
}

// conversationStreamCloseReason tells a denial apart from a failure. A
// denial is a decision the client must respect; a failure is transient and
// the client may reconnect.
func conversationStreamCloseReason(err error) string {
	var native *nativeError
	switch {
	case errors.Is(err, tenantauth.ErrUnavailable):
		return conversationClosedServerError
	case errors.As(err, &native):
		return conversationClosedAccessRevoked
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, auth.ErrHostedIdentity), errors.Is(err, accessstore.ErrDenied), errors.Is(err, operatortool.ErrAccessDenied):
		return conversationClosedAccessRevoked
	default:
		return conversationClosedServerError
	}
}
