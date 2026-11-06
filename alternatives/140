package activation

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

func TestListenerActivationHelper(t *testing.T) {
	if os.Getenv("DETENT_TEST_ACTIVATION") != "1" {
		return
	}
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	var config net.ListenConfig
	listener, err := Listen(t.Context(), "tcp", os.Getenv("DETENT_TEST_ADDRESS"), config.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("ready")
	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	request, err := http.ReadRequest(bufio.NewReader(connection))
	if err != nil {
		t.Fatal(err)
	}
	request.Body.Close()
	if _, err := fmt.Fprint(connection, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}

func TestListenerUsesSystemdDescriptor(t *testing.T) {
	t.Parallel()
	owner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	file, err := owner.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestListenerActivationHelper$")
	command.ExtraFiles = []*os.File{file}
	command.Env = []string{"DETENT_TEST_ACTIVATION=1", "DETENT_TEST_ADDRESS=" + owner.Addr().String(), "LISTEN_FDS=1", "GOCOVERDIR=" + t.TempDir()}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			command.Process.Kill()
			command.Wait()
		}
	})
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatalf("helper readiness = %q, %v", scanner.Text(), scanner.Err())
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+owner.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("activation response = %d", response.StatusCode)
	}
	for scanner.Scan() {
	}
	err = command.Wait()
	finished = true
	if err != nil {
		t.Fatal(err)
	}
}

func TestListenerRetainsQueuedRequestsAcrossRestart(t *testing.T) {
	t.Parallel()
	owner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	file := func() *os.File {
		t.Helper()
		f, err := owner.(*net.TCPListener).File()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	for _, phase := range []string{"initial startup", "restart", "rollback"} {
		t.Run(phase, func(t *testing.T) {
			var connections []net.Conn
			for _, path := range []string{"/api/v2/projects", "/mcp", "/machines/example/heartbeat", "/claims"} {
				connection, err := net.Dial("tcp", owner.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { connection.Close() })
				if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: example.test\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", path); err != nil {
					t.Fatal(err)
				}
				connections = append(connections, connection)
			}
			listener, err := inherit(file(), "tcp", owner.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})}
			done := make(chan error, 1)
			go func() { done <- server.Serve(listener) }()
			t.Cleanup(func() {
				server.Close()
				if err := <-done; err != http.ErrServerClosed {
					t.Errorf("serve: %v", err)
				}
			})
			for _, connection := range connections {
				response, err := http.ReadResponse(bufio.NewReader(connection), nil)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != http.StatusNoContent {
					t.Fatalf("queued request status = %d", response.StatusCode)
				}
			}
		})
	}
}

func TestListenerRejectsWrongActivationAddress(t *testing.T) {
	t.Parallel()
	owner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, test := range []struct{ network, address string }{
		{"tcp", "0.0.0.0:1"},
		{"tcp", "127.0.0.1:1"},
		{"unix", "/example.sock"},
	} {
		f, err := owner.(*net.TCPListener).File()
		if err != nil {
			t.Fatal(err)
		}
		listener, err := inherit(f, test.network, test.address)
		if err == nil {
			listener.Close()
			t.Fatalf("accepted %s %s", test.network, test.address)
		}
	}
}

func TestListenerWithoutActivationUsesConfiguredListen(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	called := false
	want := fmt.Errorf("fixture bind failure")
	_, err := Listen(t.Context(), "tcp", "127.0.0.1:0", func(ctx context.Context, network, address string) (net.Listener, error) {
		called = ctx == t.Context() && network == "tcp" && address == "127.0.0.1:0"
		return nil, want
	})
	if err != want || !called {
		t.Fatalf("fallback = %v, called = %t", err, called)
	}
}
