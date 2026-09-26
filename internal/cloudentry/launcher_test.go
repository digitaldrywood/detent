//go:build !windows

package cloudentry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecLauncherWritesPrivateFilesAndStops(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	launcher := &ExecLauncher{Binary: "/usr/bin/false", Configure: func(spec TenantSpec) ([]byte, error) {
		return []byte("organization_id: " + spec.Organization.ID + "\n"), nil
	}}
	spec := TenantSpec{Organization: Organization{ID: "org_exec"}, Directory: directory, Socket: filepath.Join(directory, "t.sock")}
	if err := launcher.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := launcher.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tenant.yaml", "admin-token"} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, %v", name, info, err)
		}
	}
	token, err := tenantAdminToken(directory)
	if err != nil {
		t.Fatal(err)
	}
	again, err := tenantAdminToken(directory)
	if err != nil || again != token {
		t.Fatal("tenant admin token was not reused")
	}
	if err := launcher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := launcher.Stop("org_exec"); err != nil {
		t.Fatal(err)
	}
}
