package runner

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/testenv"
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
		cancel      bool
	}{
		{name: "holds refreshes and releases", wantMethods: []string{"POST", "PUT", "DELETE"}},
		{name: "caller cancellation preserves hold until release", cancel: true, wantMethods: []string{"POST", "PUT", "DELETE"}},
		{name: "create refused", status: map[string]int{"POST": http.StatusServiceUnavailable}, wantErr: true, wantMethods: []string{"POST"}},
		{name: "refresh failure is reported", status: map[string]int{"PUT": http.StatusNotFound}, wantFailed: true, wantMethods: []string{"POST", "PUT", "DELETE"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var methods []string
			var created string
			var task struct {
				Name string `json:"name"`
			}
			refreshed := make(chan struct{}, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				methods = append(methods, r.Method)
				if r.Method == http.MethodPost {
					created = string(body)
					if err := json.Unmarshal(body, &task); err != nil {
						t.Error(err)
					}
				} else if r.URL.Path != "/v1/tasks/"+task.Name {
					t.Errorf("task path = %q, want created task %q", r.URL.Path, task.Name)
				}
				mu.Unlock()
				if r.Host != "sprite" {
					t.Errorf("host = %q, want sprite", r.Host)
				}
				if code := tt.status[r.Method]; code != 0 {
					w.WriteHeader(code)
				}
				if r.Method == http.MethodPut {
					if string(body) != `{"expire":"5m"}` {
						t.Errorf("refresh body = %q", body)
					}
					select {
					case refreshed <- struct{}{}:
					default:
					}
				}
			}))
			defer server.Close()
			dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", server.Listener.Addr().String())
			}
			failed := make(chan struct{}, 1)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			release, err := holdSpriteTaskWith(ctx, dial, 5*time.Millisecond, func() { failed <- struct{}{} })
			if (err != nil) != tt.wantErr {
				t.Fatalf("hold error = %v, want error %t", err, tt.wantErr)
			}
			if err == nil {
				if tt.cancel {
					cancel()
				}
				select {
				case <-refreshed:
				case <-testenv.WaitContext(t, t.Context()).Done():
					t.Fatal("task was never refreshed")
				}
				if tt.wantFailed {
					select {
					case <-failed:
					case <-testenv.WaitContext(t, t.Context()).Done():
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
