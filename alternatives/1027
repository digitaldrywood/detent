package global

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashboardAccessConfigRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	cfg, err := DefaultAt(path)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	cfg.DashboardAccess = DashboardAccess{
		Mode:       DashboardAccessModePrivateToken,
		Token:      token,
		AllowWrite: true,
	}
	if err := Write(path, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.DashboardAccess != cfg.DashboardAccess {
		t.Fatalf("DashboardAccess = %#v, want %#v", got.DashboardAccess, cfg.DashboardAccess)
	}
}

func TestDashboardAccessConfigValidation(t *testing.T) {
	t.Parallel()

	validToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "mapping",
			config: "dashboard_access: private",
			want:   "dashboard_access: must be a mapping",
		},
		{
			name: "mode",
			config: `dashboard_access:
  mode: password
  token: ` + validToken,
			want: "dashboard_access.mode: must equal private_token",
		},
		{
			name: "token length",
			config: `dashboard_access:
  mode: private_token
  token: short`,
			want: "dashboard_access.token: must be a 256-bit unpadded URL-safe base64 value",
		},
		{
			name: "allow write type",
			config: `dashboard_access:
  mode: private_token
  token: ` + validToken + `
  allow_write: sometimes`,
			want: "dashboard_access.allow_write: must be a boolean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(`apiVersion: detent/v1
kind: GlobalConfig
`+tt.config+`
global:
  max_concurrent_agents: 8
  scheduling: weighted
projects: []
`), tt.name+".yaml")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
