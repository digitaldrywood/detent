package conversations

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestConversationStreamBoundsEventRevalidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"valid", nil},
		{"revoked", operatortool.ErrAccessDenied},
		{"unavailable", operatortool.ErrServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				response := httptest.NewRecorder()
				stream := NewConversationStream(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response))
				checks := 0
				stream.Authorize = func() error {
					checks++
					if checks > 1 {
						return tc.err
					}
					return nil
				}
				event := conversation.Event{Seq: 1, Type: conversation.EventConversationUpdated, Data: json.RawMessage(`{}`)}
				for range 2 {
					if err := stream.Event(event); err != nil {
						t.Fatal(err)
					}
				}
				time.Sleep(ConversationAuthorizeInterval - time.Nanosecond)
				if err := stream.Event(event); err != nil {
					t.Fatal(err)
				}
				if checks != 1 {
					t.Fatalf("authorization checks before interval = %d, want 1", checks)
				}
				time.Sleep(time.Nanosecond)
				before := response.Body.Len()
				if err := stream.Event(event); !errors.Is(err, tc.err) || checks != 2 {
					t.Fatalf("authorization at interval: checks=%d error=%v, want 2 and %v", checks, err, tc.err)
				}
				if tc.err != nil {
					time.Sleep(ConversationAuthorizeInterval)
					if err := stream.Event(event); !errors.Is(err, tc.err) || checks != 2 || response.Body.Len() != before {
						t.Fatalf("failed authorization allowed delivery or revalidation: checks=%d error=%v body=%s", checks, err, response.Body.String())
					}
				}
			})
		})
	}
}
