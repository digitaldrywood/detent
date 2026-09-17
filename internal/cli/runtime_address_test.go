package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/instancelock"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func TestDashboardUsesHeldListenerAddress(t *testing.T) {
	for _, tt := range []struct {
		name    string
		host    string
		port    int
		portSet bool
		want    string
	}{
		{"runtime overrides default", "", -1, false, "100.111.222.33:4303"},
		{"explicit host", "127.0.0.8", -1, false, "127.0.0.8:4303"},
		{"explicit port", "", 4202, true, "100.111.222.33:4202"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "detent.db.lock")
			lock, err := instancelock.Acquire(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Close(); err != nil {
					t.Error(err)
				}
			})
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = file.WriteString("dashboard_address=100.111.222.33:4303\n")
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("metadata: %v, %v", err, closeErr)
			}
			opts := dashboardAddressOptions(globalconfig.Config{})
			opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
				return globalconfig.PathResolution{Path: filepath.Join(root, "global.yaml")}, nil
			}
			_, address, err := resolveDashboardBoot(t.Context(), filepath.Join(root, "global.yaml"), tt.host, tt.port, tt.portSet, opts)
			if err != nil {
				t.Fatal(err)
			}
			if address.Value != tt.want {
				t.Fatalf("address = %s, want %s", address.Value, tt.want)
			}
			if tt.host == "" && !tt.portSet {
				if check := checkDoctorDashboardAddress(BootConfig{}, address); check.Status != doctorFail {
					t.Fatalf("doctor = %+v", check)
				}
			}
		})
	}
}

func TestUpdateThroughDiscoveredListener(t *testing.T) {
	for _, tt := range []struct {
		name                  string
		released, nonLoopback bool
	}{
		{"held", false, false}, {"released", true, false}, {"non-loopback", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var applied atomic.Bool
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/state":
					_, _ = w.Write([]byte(`{"update":{"active_attempts":0}}`))
				case "/api/v1/update/apply":
					applied.Store(true)
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"current_version":"v0.114.15","action":"updated"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			if tt.nonLoopback {
				if err := server.Listener.Close(); err != nil {
					t.Fatal(err)
				}
				server.Listener = nonLoopbackListener(t)
			}
			server.Start()
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			lock, err := instancelock.Acquire(filepath.Join(root, "detent.db.lock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := lock.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := lock.SetDashboardAddress(target.Host); err != nil {
				t.Fatal(err)
			}
			if tt.released {
				if err := lock.Close(); err != nil {
					t.Fatal(err)
				}
			}
			opts := dashboardAddressOptions(globalconfig.Config{})
			opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
				return globalconfig.PathResolution{Path: filepath.Join(root, "global.yaml")}, nil
			}
			opts.httpDo = server.Client().Do
			client, err := newDashboardReadClient(t.Context(), "", "", -1, false, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tt.released {
				if client.baseURL.Host == target.Host {
					t.Fatal("released lock address used")
				}
				return
			}
			status, err := client.applyRunningUpdate(t.Context(), detentupdate.ApplyOptions{AssumeYes: true})
			if err != nil {
				t.Fatal(err)
			}
			if !applied.Load() || status.CurrentVersion != "v0.114.15" {
				t.Fatalf("applied=%t status=%+v", applied.Load(), status)
			}
			if !strings.Contains(client.address.String(), "running listener") {
				t.Fatal(client.address)
			}
		})
	}
}
