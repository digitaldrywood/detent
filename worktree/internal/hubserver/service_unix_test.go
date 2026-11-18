//go:build !windows

package hubserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

func privateSocketDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "hub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestRunServesPrivateUnixSocket(t *testing.T) {
	t.Parallel()
	directory := privateSocketDirectory(t)
	socket := filepath.Join(directory, "t.sock")
	if len(socket) >= len(syscall.RawSockaddrUnix{}.Path) {
		t.Skip("worker-owned absolute socket path exceeds the platform Unix socket limit")
	}
	seed := openTestService(t, Config{})
	cfg := seed.config
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "unix:" + socket
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		done <- Run(ctx, cfg)
	}()
	t.Cleanup(func() { cancel(); <-joined })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	testenv.WaitUntil(t, t.Context(), func() error {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://hub/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		authorizeHubTestRequest(request)
		response, err := client.Do(request)
		if err != nil {
			select {
			case serveErr := <-done:
				t.Fatalf("socket never served: %v", serveErr)
			default:
			}
			return fmt.Errorf("socket never served: %w", err)
		}
		if err := response.Body.Close(); err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("socket health status = %d", response.StatusCode)
		}
		return nil
	})
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v", info, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrepareUnixListener(t *testing.T) {
	t.Parallel()
	absolute := privateSocketDirectory(t)
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	private, err := filepath.Rel(working, absolute)
	if err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(private, "public", "live.sock")) >= len(syscall.RawSockaddrUnix{}.Path) {
		t.Skip("worker-owned socket path exceeds the platform Unix socket limit")
	}
	public := filepath.Join(private, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(private, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(private, "stale.sock")
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(private, "live.sock")
	liveListener, err := listenConfig.Listen(t.Context(), "unix", live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = liveListener.Close() })
	for _, test := range []struct {
		name string
		path string
		ok   bool
	}{
		{"fresh path", filepath.Join(private, "fresh.sock"), true},
		{"stale socket", stale, true},
		{"public directory", filepath.Join(public, "x.sock"), false},
		{"regular file", regular, false},
		{"live socket", live, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := prepareUnixListener(t.Context(), test.path); (err == nil) != test.ok {
				t.Fatalf("prepareUnixListener error = %v, want ok %v", err, test.ok)
			}
		})
	}
	for _, address := range []string{"unix:relative.sock", "unix:/tmp/../x.sock"} {
		if _, ok := listenerUnixPath(address); ok {
			t.Fatalf("listenerUnixPath accepted %q", address)
		}
	}
}
