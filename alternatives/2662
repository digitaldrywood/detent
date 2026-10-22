package profiling

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestServiceReload(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	for _, withListener := range []bool{false, true} {
		name := "capture"
		if withListener {
			name = "listener and capture"
		}
		t.Run(name, func(t *testing.T) { testServiceReload(t, withListener) })
	}
}

func testServiceReload(t *testing.T, withListener bool) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	service := New(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(service.Close)
	old := filepath.Join(dir, time.Now().Add(-200*time.Hour).UTC().Format(bundleTimeFormat))
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	service.Apply(ctx, Config{})
	if service.listener != nil || service.cancelCapture != nil {
		t.Fatal("profiling enabled by default")
	}
	eventually(t, func() bool { _, err := os.Stat(old); return os.IsNotExist(err) })
	config := Default()
	if withListener {
		config.ListenAddr = "127.0.0.1:0"
	}
	config.Capture.Enabled = true
	config.Capture.Interval = 300 * time.Millisecond
	config.Capture.CPUDuration = time.Millisecond
	if err := os.Mkdir(old, 0o700); err != nil {
		t.Fatal(err)
	}
	service.Apply(ctx, config)
	if withListener && service.listener == nil {
		t.Fatal("profiling listener did not start")
	}
	if runtime.SetMutexProfileFraction(-1) != 5 {
		t.Fatal("mutex sampling not enabled")
	}
	if withListener {
		assertEndpoint(t, service.listener.Addr().String())
	}
	eventually(t, func() bool { _, err := os.Stat(old); return os.IsNotExist(err) })
	eventually(t, func() bool {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", "block.pprof"))
		return len(matches) > 0
	})
	listenerDone, captureDone := service.listenerDone, service.captureDone
	service.Apply(ctx, config)
	if service.listenerDone != listenerDone || service.captureDone != captureDone {
		t.Fatal("unchanged config restarted profiling")
	}
	if withListener {
		config.ListenAddr = "localhost:0"
	}
	config.Capture.Dir = t.TempDir()
	config.Capture.Interval = 400 * time.Millisecond
	service.Apply(ctx, config)
	if withListener {
		select {
		case <-listenerDone:
		default:
			t.Fatal("old listener not joined")
		}
	}
	select {
	case <-captureDone:
	default:
		t.Fatal("old capture loop not joined")
	}
	if withListener {
		if service.listener == nil {
			t.Fatal("profiling listener did not reconfigure")
		}
		assertEndpoint(t, service.listener.Addr().String())
	}
	eventually(t, func() bool {
		matches, _ := filepath.Glob(filepath.Join(config.Capture.Dir, "*", "heap.pprof"))
		return len(matches) > 0
	})
	invalid := config
	invalid.ListenAddr = "0.0.0.0:0"
	listener := service.listener
	service.Apply(ctx, invalid)
	if service.listener != listener {
		t.Fatal("invalid reload replaced working listener")
	}
	service.Apply(ctx, Config{})
	if service.listener != nil || service.cancelCapture != nil {
		t.Fatal("disable left profiling running")
	}
	if runtime.SetMutexProfileFraction(-1) != 0 {
		t.Fatal("disable left mutex sampling enabled")
	}
	service.Apply(ctx, config)
	cancel()
	eventually(t, func() bool {
		select {
		case <-service.captureDone:
			return true
		default:
			return false
		}
	})
	service.Close()
	if runtime.SetMutexProfileFraction(-1) != 0 {
		t.Fatal("shutdown left sampling enabled")
	}
}

func assertEndpoint(t *testing.T, address string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/debug/pprof/heap", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pprof status = %d", response.StatusCode)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true")
		case <-ticker.C:
		}
	}
}
