package dispatch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestConversationControlsOutlivesDefaultRequestTimeout proves the long poll
// carries its own deadline. The dogfood run of September 11, 2026 configured
// nothing, so the client's default request timeout (10s) bounded a 25s poll
// and every control poll died with "context deadline exceeded"; steering,
// answering and interrupting never reached the bound attempt. The fix must be
// a per-request deadline, not a global bump, so an ordinary request through
// the same client still honours the default.
func TestConversationControlsOutlivesDefaultRequestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	const (
		clientTimeout = 60 * time.Millisecond
		serverDelay   = 250 * time.Millisecond
	)

	var waits []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		waits = append(waits, r.URL.Query().Get("wait"))
		mu.Unlock()
		select {
		case <-time.After(serverDelay):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"controls":[{"cursor":4,"key":"k4","kind":"message","message_id":"msg_4","text":"steer"}],"cursor":4}`))
	}))
	t.Cleanup(server.Close)

	transport := server.Client()
	transport.Timeout = clientTimeout
	client, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: transport})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	identity := ConversationIdentity{LeaseID: "lease-1", FencingToken: 7, AttemptID: "attempt_1"}

	page, err := native.ConversationControls(t.Context(), "conv_1", identity, 0, 2*time.Second)
	if err != nil {
		t.Fatalf("ConversationControls() error = %v, want the poll to outlive the client timeout", err)
	}
	if len(page.Controls) != 1 || page.Controls[0].Key != "k4" || page.Cursor != 4 {
		t.Fatalf("ConversationControls() page = %#v", page)
	}

	// The client's own default still bounds a request that is not a long
	// poll, so the fix did not raise the timeout for everything.
	if _, err := native.ConversationControls(t.Context(), "conv_1", identity, 0, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("non-polling request error = %v, want the client timeout to still apply", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 2 || waits[0] != "2" || waits[1] != "0" {
		t.Fatalf("observed waits = %v", waits)
	}
}
