package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/web"
)

func TestRuntimeCanceledDialShutdown(t *testing.T) {
	tests := []struct {
		name            string
		closeClient     bool
		closeBeforeDial bool
		wantErr         error
	}{
		{name: "retained connection expires web shutdown", wantErr: context.DeadlineExceeded},
		{name: "close completed dial before runtime cancellation", closeClient: true, wantErr: context.Canceled},
		{name: "close pending dial before runtime cancellation", closeClient: true, closeBeforeDial: true, wantErr: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server, err := web.NewServer(web.Config{Mode: web.ModeOnboarding}, web.Dependencies{})
				if err != nil {
					t.Fatal(err)
				}
				clientConn, serverConn := net.Pipe()
				defer clientConn.Close()
				defer serverConn.Close()
				listener := &runtimePipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
				listener.connections <- serverConn
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				runtimeDone := make(chan error, 1)
				go func() { runtimeDone <- serve(ctx, server, listener) }()

				dialStarted := make(chan struct{})
				releaseDial := make(chan struct{})
				transport := &http.Transport{
					DialContext: func(context.Context, string, string) (net.Conn, error) {
						close(dialStarted)
						<-releaseDial
						return clientConn, nil
					},
				}
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: transport}
				requestCtx, cancelRequest := context.WithCancel(context.Background())
				defer cancelRequest()
				request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://runtime.test/health", nil)
				if err != nil {
					t.Fatal(err)
				}
				requestDone := make(chan error, 1)
				go func() {
					response, err := client.Do(request)
					if response != nil {
						_ = response.Body.Close()
					}
					requestDone <- err
				}()
				<-dialStarted
				cancelRequest()
				if err := <-requestDone; !errors.Is(err, context.Canceled) {
					t.Fatalf("request error = %v, want context canceled", err)
				}
				if tt.closeBeforeDial {
					client.CloseIdleConnections()
				}
				close(releaseDial)
				synctest.Wait()
				if tt.closeClient && !tt.closeBeforeDial {
					client.CloseIdleConnections()
					synctest.Wait()
				}
				cancel()
				if err := <-runtimeDone; !errors.Is(err, tt.wantErr) {
					t.Fatalf("serve() error = %v, want %v", err, tt.wantErr)
				}
			})
		})
	}
}

func TestRuntimeHTTPHelpersCloseConnections(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		request func(*testing.T, string, <-chan error)
	}{
		{name: "dashboard", status: http.StatusOK, request: func(t *testing.T, rawURL string, done <-chan error) {
			waitForDashboard(t, rawURL, done)
		}},
		{name: "scenario", status: http.StatusOK, request: func(t *testing.T, rawURL string, done <-chan error) {
			waitForDashboardHeader(t, rawURL, done, "example")
		}},
		{name: "refresh", status: http.StatusAccepted, request: postRuntimeRefresh},
		{name: "kanban", status: http.StatusOK, request: func(t *testing.T, rawURL string, done <-chan error) {
			postRuntimeKanbanForm(t, rawURL, done, url.Values{"project_id": {"example"}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := make(chan struct{})
			var closeOnce sync.Once
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closeOnce.Do(func() { close(closed) })
				}
			}
			server.Start()
			defer server.Close()
			tt.request(t, server.URL, make(chan error))
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Fatal("HTTP helper returned without closing its connection")
			}
		})
	}
}

func TestRuntimeHTTPClientOwnsTransport(t *testing.T) {
	first := newRuntimeHTTPClient()
	defer first.CloseIdleConnections()
	second := newRuntimeHTTPClient()
	defer second.CloseIdleConnections()
	if first.Transport == nil || second.Transport == nil || first.Transport == second.Transport || first.Transport == http.DefaultTransport || second.Transport == http.DefaultTransport {
		t.Fatal("runtime HTTP clients must own independent transports")
	}
}

type runtimePipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func (l *runtimePipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *runtimePipeListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*runtimePipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}
