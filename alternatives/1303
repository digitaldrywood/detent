package nativechange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/testenv"
)

// TestConversationControlPollWarnsOncePerMinute proves a poll that keeps
// failing writes one warning a minute, not one per poll. The failing poll
// repeats every backoff, so a line per attempt buries everything else in the
// runner log.
func TestConversationControlPollWarnsOncePerMinute(t *testing.T) {
	warnings := captureWarnings(t)

	hub, execution := newConversationHub(t)
	hub.denyControls(http.StatusInternalServerError)
	session, err := execution.BindConversation(t.Context(), runner.ConversationCapabilities{Steer: true}, "")
	if err != nil {
		t.Fatal(err)
	}

	testenv.WaitUntil(t, t.Context(), func() error {
		if count := hub.denyCount(); count < 4 {
			return fmt.Errorf("the poll retried %d times, want at least 4", count)
		}
		return nil
	})

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
