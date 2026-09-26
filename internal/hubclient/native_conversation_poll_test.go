package hubclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

// TestConversationControlsOutlivesDefaultRequestTimeout proves the long poll
// carries its own deadline. The dogfood run of September 11, 2026 configured
// nothing, so the client's default request timeout (10s) bounded a 25s poll
// and every control poll died with "context deadline exceeded"; steering,
// answering and interrupting never reached the bound attempt. The fix must be
// a per-request deadline, not a global bump, so an ordinary request through
// the same client still honours the default.
func TestConversationControlsOutlivesDefaultRequestTimeout(t *testing.T) {
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
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "test" }, HTTPClient: transport})
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

func TestConversationPollTimeoutAddsSlackToWait(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		wait time.Duration
		want time.Duration
	}{
		{name: "no wait keeps the client default", wait: 0, want: 0},
		{name: "negative wait keeps the client default", wait: -time.Second, want: 0},
		{name: "production wait gets slack", wait: 25 * time.Second, want: 25*time.Second + conversationPollSlack},
		{name: "short wait gets slack", wait: time.Second, want: time.Second + conversationPollSlack},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := conversationPollTimeout(test.wait); got != test.want {
				t.Fatalf("conversationPollTimeout(%v) = %v, want %v", test.wait, got, test.want)
			}
		})
	}
}

// TestConversationControlPollWarnsOncePerMinute proves a poll that keeps
// failing writes one warning a minute, not one per poll. The failing poll
// repeats every backoff, so a line per attempt buries everything else in the
// runner log.
func TestConversationControlPollWarnsOncePerMinute(t *testing.T) {
	useFastConversationTimings(t)
	warnings := captureWarnings(t)

	hub, execution := newConversationHub(t)
	hub.denyControls(http.StatusInternalServerError)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true})
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for hub.denyCount() < 4 {
		select {
		case <-deadline:
			t.Fatalf("the poll retried %d times, want at least 4", hub.denyCount())
		case <-time.After(5 * time.Millisecond):
		}
	}

	got := warnings.count("conversation control poll failed")
	if got != 1 {
		t.Fatalf("poll failure warnings = %d after %d failed polls, want 1 per minute", got, hub.denyCount())
	}

	if err := session.Close(context.WithoutCancel(t.Context()), runner.ConversationOutcomeFailed, errors.New("scripted")); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// warningRecorder collects the warn-level messages the session logs.
type warningRecorder struct {
	mu       sync.Mutex
	messages []string
}

func (r *warningRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *warningRecorder) Handle(_ context.Context, record slog.Record) error {
	if record.Level < slog.LevelWarn {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, record.Message)
	return nil
}

func (r *warningRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *warningRecorder) WithGroup(string) slog.Handler { return r }

func (r *warningRecorder) count(message string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := 0
	for _, seen := range r.messages {
		if seen == message {
			total++
		}
	}
	return total
}

// captureWarnings redirects the default logger, which is where a conversation
// session takes its own logger from.
func captureWarnings(t *testing.T) *warningRecorder {
	t.Helper()
	recorder := &warningRecorder{}
	previous := slog.Default()
	slog.SetDefault(slog.New(recorder))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return recorder
}
