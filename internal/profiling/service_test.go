package profiling

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServiceReload(t *testing.T) {
	service := New(t.Context(), t.TempDir(), "runner", slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(service.Close)
	expired := filepath.Join(service.defaultDir, time.Now().Add(-200*time.Hour).UTC().Format(bundleTimeFormat))
	if err := os.MkdirAll(expired, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := service.Apply(Config{}); err != nil {
		t.Fatal(err)
	}
	<-service.captureDone
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("disabled startup retained an expired bundle: %v", err)
	}
	if service.listener != nil || service.capture.Enabled {
		t.Fatal("disabled profiling started a listener or periodic capture")
	}
	var previous string
	for _, address := range []string{"127.0.0.1:0", "localhost:0", ""} {
		if err := service.Apply(Config{ListenAddr: address}); err != nil {
			t.Fatal(err)
		}
		if previous != "" {
			connection, err := net.DialTimeout("tcp", previous, time.Second)
			if err == nil {
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
				t.Fatal("old profiling listener still accepts connections")
			}
		}
		if address == "" {
			break
		}
		previous = service.listener.Addr().String()
		client := &http.Client{Timeout: 3 * time.Second}
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap"} {
			response, err := client.Get("http://" + previous + path)
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if response.StatusCode != http.StatusOK || readErr != nil || closeErr != nil {
				t.Fatalf("pprof status=%d read=%v close=%v", response.StatusCode, readErr, closeErr)
			}
		}
		client.CloseIdleConnections()
		occupied, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Apply(Config{ListenAddr: occupied.Addr().String()}); err == nil {
			t.Fatal("occupied listener succeeded")
		}
		if err := occupied.Close(); err != nil {
			t.Fatal(err)
		}
		if service.listener.Addr().String() != previous {
			t.Fatal("failed reload replaced healthy listener")
		}
	}
	for _, dir := range []string{service.defaultDir, t.TempDir()} {
		cfg := Config{Capture: DefaultCapture()}
		cfg.Capture.Enabled = true
		cfg.Capture.Interval = 20 * time.Millisecond
		cfg.Capture.CPUDuration = time.Millisecond
		if dir != service.defaultDir {
			cfg.Capture.Dir = dir
			cfg.Capture.Interval = 40 * time.Millisecond
			cfg.Capture.CPUDuration = 2 * time.Millisecond
		}
		old := filepath.Join(dir, time.Now().Add(-200*time.Hour).UTC().Format(bundleTimeFormat))
		if err := os.MkdirAll(old, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := service.Apply(cfg); err != nil {
			t.Fatal(err)
		}
		waitProfileCondition(t, func() bool {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return false
			}
			for _, entry := range entries {
				if _, err := os.Stat(filepath.Join(dir, entry.Name(), "block.pprof")); err == nil {
					return true
				}
			}
			return false
		})
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatalf("startup retention left expired bundle: %v", err)
		}
		if fraction := runtime.SetMutexProfileFraction(-1); fraction != 100 {
			t.Fatalf("mutex sampling fraction=%d", fraction)
		}
	}
	if err := service.Apply(Config{}); err != nil {
		t.Fatal(err)
	}
	if service.captureCancel != nil || runtime.SetMutexProfileFraction(-1) != 0 {
		t.Fatal("disabled capture retained sampling or its loop")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stopped := New(ctx, t.TempDir(), "hub", nil)
	defer stopped.Close()
	if err := stopped.Apply(Config{ListenAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("canceled service started profiling")
	}
}

func waitProfileCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("profiling condition timed out")
		case <-ticker.C:
		}
	}
}
