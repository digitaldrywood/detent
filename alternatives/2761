package runner

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHoldSpriteTask(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name        string
		status      map[string]int
		wantErr     bool
		wantFailed  bool
		wantMethods []string
	}{
		{"holds refreshes and releases", nil, false, false, []string{"POST", "PUT", "DELETE"}},
		{"create refused", map[string]int{"POST": http.StatusServiceUnavailable}, true, false, []string{"POST"}},
		{"refresh failure is reported", map[string]int{"PUT": http.StatusNotFound}, false, true, []string{"POST", "PUT", "DELETE"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var methods []string
			var created string
			refreshed := make(chan struct{}, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				methods = append(methods, r.Method)
				if r.Method == http.MethodPost {
					created = string(body)
				}
				mu.Unlock()
				if r.Host != "sprite" {
					t.Errorf("host = %q, want sprite", r.Host)
				}
				if code := tt.status[r.Method]; code != 0 {
					w.WriteHeader(code)
				}
				if r.Method == http.MethodPut {
					refreshed <- struct{}{}
				}
			}))
			defer server.Close()
			dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
			}
			failed := make(chan struct{}, 1)
			release, err := holdSpriteTaskWith(t.Context(), dial, 5*time.Millisecond, func() { failed <- struct{}{} })
			if (err != nil) != tt.wantErr {
				t.Fatalf("hold error = %v, want error %t", err, tt.wantErr)
			}
			if err == nil {
				select {
				case <-refreshed:
				case <-time.After(5 * time.Second):
					t.Fatal("task was never refreshed")
				}
				if tt.wantFailed {
					select {
					case <-failed:
					case <-time.After(5 * time.Second):
						t.Fatal("refresh failure was not reported")
					}
				}
				release()
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.Contains(created, `"expire":"5m"`) || !strings.Contains(created, `"name":"detent-`) {
				t.Fatalf("create body = %q", created)
			}
			compact := []string{}
			for _, method := range methods {
				if len(compact) == 0 || compact[len(compact)-1] != method {
					compact = append(compact, method)
				}
			}
			if strings.Join(compact, ",") != strings.Join(tt.wantMethods, ",") {
				t.Fatalf("methods = %v, want %v", compact, tt.wantMethods)
			}
			if !tt.wantFailed && len(failed) != 0 {
				t.Fatal("healthy hold reported a failure")
			}
		})
	}
}
