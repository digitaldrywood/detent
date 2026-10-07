//go:build !windows

package cloudentry

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
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

func TestExecLauncherJoinsTenantShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	t.Parallel()
	for _, test := range []struct {
		name    string
		tenants int
	}{{name: "Stop", tenants: 1}, {name: "Close", tenants: 2}, {name: "unreachable child", tenants: 1}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directory := t.TempDir()
			var listenConfig net.ListenConfig
			listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(directory, "tenant-fixture")
			script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = --database ]; then shift; export DETENT_TEST_LOCK=\"$1.lock\"; export GOCOVERDIR=\"$1.lock.coverage\"; fi\n  shift\ndone\nexec \"$DETENT_TEST_BINARY\" -test.run=^TestTenantShutdownHelperProcess$\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			launcher := &ExecLauncher{Binary: binary, RestartLimit: 1, Logger: slog.New(slog.DiscardHandler), Environment: []string{
				"DETENT_TEST_BINARY=" + os.Args[0], "DETENT_TEST_ADDRESS=" + listener.Addr().String(),
			}, Configure: func(TenantSpec) ([]byte, error) { return []byte("{}\n"), nil }}
			t.Cleanup(func() { _ = launcher.Close() })
			var connections []net.Conn
			var paths []string
			for i := range test.tenants {
				id := fmt.Sprintf("org_%d", i)
				tenantDirectory := filepath.Join(directory, id)
				if err := os.Mkdir(tenantDirectory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(tenantDirectory, "hub.db.lock.coverage"), 0700); err != nil {
					t.Fatal(err)
				}
				spec := TenantSpec{Organization: Organization{ID: id}, Directory: tenantDirectory}
				if test.name == "unreachable child" {
					spec.Check = func(context.Context) error { return syscall.ECONNREFUSED }
				}
				if err := launcher.Start(t.Context(), spec); err != nil {
					t.Fatal(err)
				}
				conn, err := listener.Accept()
				if err != nil {
					t.Fatal(err)
				}
				connections = append(connections, conn)
				t.Cleanup(func() { _ = conn.Close() })
				if err := conn.SetDeadline(time.Now().Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				path, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				paths = append(paths, strings.TrimSpace(path))
			}
			done := make(chan error, 1)
			launcher.mu.Lock()
			tenant := launcher.running["org_0"]
			launcher.mu.Unlock()
			go func() {
				switch test.name {
				case "Stop":
					done <- launcher.Stop("org_0")
				case "Close":
					done <- launcher.Close()
				default:
					<-tenant.done
					if !errors.Is(launcher.Failure("org_0"), syscall.ECONNREFUSED) {
						done <- fmt.Errorf("unreachable tenant failure: %w", launcher.Failure("org_0"))
						return
					}
					done <- nil
				}
			}()
			for i, conn := range connections {
				phase, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || phase != "stopping\n" {
					t.Fatalf("tenant %d shutdown = %q, %v", i, phase, err)
				}
				inspection, err := instancelock.Inspect(paths[i])
				if err != nil || inspection.Status != instancelock.StatusHeld {
					t.Fatalf("shutdown lock = %+v, %v", inspection, err)
				}
			}
			select {
			case err := <-done:
				t.Fatalf("shutdown returned before tenants released ownership: %v", err)
			default:
			}
			for _, conn := range connections {
				if _, err := conn.Write([]byte("release\n")); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("shutdown did not join tenants")
			}
			for _, path := range paths {
				inspection, err := instancelock.Inspect(path)
				if err != nil || inspection.Status != instancelock.StatusClear {
					t.Fatalf("joined lock = %+v, %v", inspection, err)
				}
			}
			if test.name == "Close" {
				if err := launcher.Start(t.Context(), TenantSpec{Organization: Organization{ID: "org_late"}}); err == nil {
					t.Fatal("closed launcher admitted another tenant")
				}
			}
		})
	}
}

func TestTenantShutdownHelperProcess(t *testing.T) {
	address := os.Getenv("DETENT_TEST_ADDRESS")
	if address == "" {
		return
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	lock, err := instancelock.Acquire(os.Getenv("DETENT_TEST_LOCK"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	dialer := net.Dialer{Timeout: 20 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(conn, os.Getenv("DETENT_TEST_LOCK")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-interrupts:
	case <-time.After(time.Minute):
		t.Fatal("tenant received no shutdown signal")
	}
	if _, err := fmt.Fprintln(conn, "stopping"); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}
