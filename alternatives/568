package cli

import (
	"os"
	"path/filepath"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/instancelock"
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
