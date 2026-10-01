package cli

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchedProfiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiling.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("profiling: {listen_addr: '127.0.0.1:0'}\n")
	cfg, err := readProfilingConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan profilingLog, 20)
	logger := slog.New(profilingLogHandler{events: events})
	stop := startWatchedProfiling(t.Context(), cfg, path, t.TempDir(), "hub", logger)
	defer stop()
	wait := func(message string) profilingLog {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case event := <-events:
				if event.message == message {
					return event
				}
			case <-deadline.C:
				t.Fatalf("timed out waiting for %q", message)
			}
		}
	}
	address := wait("profiling listener started").address
	check := func() {
		t.Helper()
		client := &http.Client{Timeout: time.Second}
		defer client.CloseIdleConnections()
		response, err := client.Get("http://" + address + "/debug/pprof/")
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil {
			t.Fatalf("pprof status=%d read=%v close=%v", response.StatusCode, readErr, closeErr)
		}
	}
	check()
	write("profiling: {listen_addr: '0.0.0.0:6060'}\n")
	wait("reload profiling config")
	check()
	write("profiling: {listen_addr: 'localhost:0'}\n")
	address = wait("profiling listener started").address
	check()
	write("profiling: {listen_addr: ''}\n")
	wait("profiling listener stopped")
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	if response, err := client.Get("http://" + address + "/debug/pprof/"); err == nil {
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("disabled profiling listener is reachable")
	}
}

type profilingLog struct {
	message string
	address string
}

type profilingLogHandler struct {
	events chan<- profilingLog
}

func (h profilingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h profilingLogHandler) Handle(_ context.Context, record slog.Record) error {
	event := profilingLog{message: record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "address" {
			event.address = attr.Value.String()
		}
		return true
	})
	h.events <- event
	return nil
}

func (h profilingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h profilingLogHandler) WithGroup(string) slog.Handler      { return h }
