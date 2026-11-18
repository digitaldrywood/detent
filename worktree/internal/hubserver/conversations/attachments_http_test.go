package conversations

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestConversationAttachmentBodyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "over the limit", err: &http.MaxBytesError{Limit: conversation.MaxAttachmentBytes}, status: http.StatusRequestEntityTooLarge},
		{name: "wrapped over the limit", err: fmt.Errorf("read part: %w", &http.MaxBytesError{Limit: 1}), status: http.StatusRequestEntityTooLarge},
		{name: "truncated", err: io.ErrUnexpectedEOF, status: http.StatusUnprocessableEntity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var failure *hubapi.NativeError
			if !errors.As(AttachmentBodyError(test.err), &failure) {
				t.Fatalf("conversationAttachmentBodyError(%v) is not an API error", test.err)
			}
			if failure.Status != test.status {
				t.Fatalf("status = %d, want %d", failure.Status, test.status)
			}
		})
	}
}
