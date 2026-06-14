package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/devruntime"
)

func TestStartIsolatedRuntimeAutoPromotesFixtureAndStopsOnCancel(t *testing.T) {
	runtime, err := devruntime.Build(devruntime.Config{Home: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatalf("devruntime.Build() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(ctx, devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), output))
	}()
	t.Cleanup(cancel)

	url := waitForIsolatedRuntimeURL(t, output, done)
	if banner := output.String(); !strings.Contains(banner, "Mode: isolated dev runtime") || !strings.Contains(banner, "DB mode: memory") {
		t.Fatalf("isolated runtime banner missing isolation details:\n%s", banner)
	}
	waitForDashboard(t, url+"/health", done)
	postRuntimeRefresh(t, url, done)

	body := waitForDashboardCondition(t, url+"/api/v1/state", done, "mock issue promoted to Merging", func(body string) bool {
		return boardStateCountFromBody(t, body, "Merging") == 1
	})
	if !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("state response missing running status:\n%s", body)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startRunning() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for isolated runtime to stop")
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForIsolatedRuntimeURL(t *testing.T, output *lockedBuffer, done <-chan error) string {
	t.Helper()

	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before banner: %v", err)
		case <-deadline:
			t.Fatalf("timed out waiting for isolated runtime banner:\n%s", output.String())
		default:
		}

		for line := range strings.SplitSeq(output.String(), "\n") {
			url, ok := strings.CutPrefix(line, "Dashboard: ")
			if ok && strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postRuntimeRefresh(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for ctx.Err() == nil {
		select {
		case err := <-done:
			t.Fatalf("isolated runtime stopped before refresh: %v", err)
		default:
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/api/v1/refresh", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext() error = %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Fatalf("Body.Close() error = %v", closeErr)
			}
			if resp.StatusCode == http.StatusAccepted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out posting runtime refresh to %s", url)
}

func boardStateCountFromBody(t *testing.T, body string, state string) int {
	t.Helper()

	var payload struct {
		Board struct {
			StateDistribution []struct {
				State string `json:"state"`
				Count int    `json:"count"`
			} `json:"state_distribution"`
		} `json:"board"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return 0
	}
	for _, entry := range payload.Board.StateDistribution {
		if entry.State == state {
			return entry.Count
		}
	}
	return 0
}
