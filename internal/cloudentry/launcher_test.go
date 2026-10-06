//go:build !windows

package cloudentry

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecLauncherWritesPrivateFilesAndStops(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{name: "without App credentials", want: "\n\n\n"},
		{name: "product App credentials", environment: []string{"DETENT_HUB_GITHUB_APP_ID=123", "DETENT_HUB_GITHUB_APP_PRIVATE_KEY=private-key", "DETENT_HUB_GITHUB_WEBHOOK_SECRET=webhook-secret"}, want: "123\nprivate-key\nwebhook-secret\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			binary := filepath.Join(directory, "tenant-fixture")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DETENT_TEST_ARGS\"\nprintf '%s\\n' \"$DETENT_HUB_GITHUB_APP_ID\" \"$DETENT_HUB_GITHUB_APP_PRIVATE_KEY\" \"$DETENT_HUB_GITHUB_WEBHOOK_SECRET\" > \"$DETENT_TEST_ENV\"\nexit 1\n"
			if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			launcher := &ExecLauncher{Binary: binary, RestartLimit: 1, Logger: slog.New(slog.DiscardHandler), Environment: append(test.environment, "DETENT_TEST_ARGS="+filepath.Join(directory, "args"), "DETENT_TEST_ENV="+filepath.Join(directory, "env")), Configure: func(spec TenantSpec) ([]byte, error) {
				return []byte("organization_id: " + spec.Organization.ID + "\n"), nil
			}}
			t.Cleanup(func() { _ = launcher.Close() })
			spec := TenantSpec{Organization: Organization{ID: "org_exec"}, Directory: directory, Socket: filepath.Join(directory, "t.sock")}
			if err := launcher.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			waitLauncherFailure(t, launcher, "org_exec")
			arguments, err := os.ReadFile(filepath.Join(directory, "args"))
			if err != nil || strings.Contains(string(arguments), "--github-disabled") {
				t.Fatalf("tenant arguments = %s: %v", arguments, err)
			}
			environment, err := os.ReadFile(filepath.Join(directory, "env"))
			if err != nil || string(environment) != test.want {
				t.Fatalf("tenant environment = %q, want %q: %v", environment, test.want, err)
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
		})
	}
}

func TestExecLauncherStopsSupervisingAfterRestartLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	t.Parallel()
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{
		{name: "configured limit", limit: 2, want: "the tenant Hub exited 2 times in a row without staying up (last: exit status 1)"},
		{name: "single exit", limit: 1, want: "the tenant Hub exited once without staying up (last: exit status 1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			launcher := &ExecLauncher{Binary: "/usr/bin/false", RestartLimit: tc.limit, Logger: slog.New(slog.DiscardHandler), Configure: func(TenantSpec) ([]byte, error) { return []byte("{}\n"), nil }}
			spec := TenantSpec{Organization: Organization{ID: "org_exec"}, Directory: directory, Socket: filepath.Join(directory, "t.sock")}
			if err := launcher.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			failure := waitLauncherFailure(t, launcher, "org_exec")
			var exit *TenantExitError
			if !errors.As(failure, &exit) || exit.Exits != tc.limit || failure.Error() != tc.want {
				t.Fatalf("Failure() = %v, want %q", failure, tc.want)
			}
			launcher.mu.Lock()
			_, running := launcher.running["org_exec"]
			launcher.mu.Unlock()
			if running {
				t.Fatal("a tenant that exhausted its restart limit is still supervised")
			}
			if err := launcher.Start(t.Context(), spec); err != nil {
				t.Fatal(err)
			}
			if launcher.Failure("org_exec") != nil {
				t.Fatal("a fresh start kept the previous failure")
			}
			if err := launcher.Close(); err != nil {
				t.Fatal(err)
			}
			if launcher.Failure("org_exec") != nil {
				t.Fatal("a stopped tenant kept its failure")
			}
		})
	}
}

func waitLauncherFailure(t *testing.T, launcher *ExecLauncher, id string) error {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if failure := launcher.Failure(id); failure != nil {
			return failure
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor never stopped restarting a tenant that exits immediately")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecLauncherKeepsRestartingReadyTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("live service integration")
	}

	t.Parallel()
	directory := t.TempDir()
	launcher := &ExecLauncher{Binary: "/usr/bin/false", RestartLimit: 1, Logger: slog.New(slog.DiscardHandler), Configure: func(TenantSpec) ([]byte, error) { return []byte("{}\n"), nil }}
	spec := TenantSpec{Organization: Organization{ID: "org_ready", State: "ready"}, Directory: directory, Socket: filepath.Join(directory, "t.sock")}
	if err := launcher.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launcher.Close() })
	time.Sleep(2500 * time.Millisecond)
	if failure := launcher.Failure("org_ready"); failure != nil {
		t.Fatalf("a ready tenant gave up after a crash burst: %v", failure)
	}
	launcher.mu.Lock()
	_, running := launcher.running["org_ready"]
	launcher.mu.Unlock()
	if !running {
		t.Fatal("a ready tenant stopped being supervised")
	}
}
