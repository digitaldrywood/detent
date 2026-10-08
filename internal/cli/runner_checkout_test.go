package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPrepareRunnerCheckout(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		existing  string
		url       string
		fail      bool
		wantClone bool
		wantError bool
	}{
		{name: "missing checkout", url: "https://github.com/acme/orders.git", wantClone: true},
		{name: "existing checkout", existing: ".git", url: "https://github.com/acme/orders.git"},
		{name: "occupied directory", existing: "operator-file", url: "https://github.com/acme/orders.git", wantError: true},
		{name: "unbound project", wantError: true},
		{name: "URL credentials refused", url: "https://secret@github.com/acme/orders.git", wantError: true},
		{name: "clone failure is retryable", url: "https://github.com/acme/orders.git", fail: true, wantClone: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			selected := globalconfig.Project{Workdir: filepath.Join(t.TempDir(), "orders")}
			if test.existing != "" {
				if err := os.MkdirAll(selected.Workdir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(selected.Workdir, test.existing), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			clone := func(_ context.Context, url, target string) error {
				calls++
				if url != test.url || target == selected.Workdir {
					t.Fatalf("unsafe clone destination %q, URL %q", target, url)
				}
				if err := os.WriteFile(filepath.Join(target, "source"), []byte("cloned"), 0600); err != nil {
					return err
				}
				if test.fail {
					return errors.New("private git output")
				}
				return nil
			}
			err := prepareRunnerCheckoutWithClone(t.Context(), selected, test.url, clone)
			if (err != nil) != test.wantError || (calls > 0) != test.wantClone {
				t.Fatalf("clone calls %d, error %v", calls, err)
			}
			if test.existing != "" {
				body, err := os.ReadFile(filepath.Join(selected.Workdir, test.existing))
				if err != nil || string(body) != "keep" {
					t.Fatalf("existing file changed: %q %v", body, err)
				}
			}
			if test.fail {
				if _, err := os.Lstat(selected.Workdir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial checkout remains: %v", err)
				}
				test.fail = false
				if err := prepareRunnerCheckoutWithClone(t.Context(), selected, test.url, clone); err != nil {
					t.Fatalf("resume clone: %v", err)
				}
			}
			leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(selected.Workdir), ".runner-checkout-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("clone scratch remains: %v %v", leftovers, err)
			}
		})
	}
}

func TestHubRunnerRegisterStartsWithoutCheckouts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		service    bool
		startError bool
	}{
		{name: "service starts", service: true},
		{name: "foreground instructions"},
		{name: "service error returned", service: true, startError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
			root := t.TempDir()
			config := filepath.Join(root, "config", "global.yaml")
			args := []string{"--url", hub.server.URL + "/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", config, "--workspace-root", filepath.Join(root, "checkouts")}
			if test.service {
				args = append(args, "--service")
			}
			started := false
			output, err := runRegisterInTestWorkspace(t, nil, func(_ *cobra.Command, path string) error {
				started = true
				if path != config {
					t.Fatalf("service config %q", path)
				}
				if test.startError {
					return errors.New("service install failed")
				}
				return nil
			}, args...)
			if (err != nil) != test.startError || started != test.service {
				t.Fatalf("started %v, error %v: %s", started, err, output)
			}
			if strings.Contains(output, "Clone the") || test.service && !test.startError && strings.Contains(output, "next_steps") {
				t.Fatalf("manual setup steps: %s", output)
			}
			if !test.service && !strings.Contains(output, "detent start --config") {
				t.Fatalf("missing start command: %s", output)
			}
			if _, err := os.Lstat(filepath.Join(root, "checkouts", "orders")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("register created checkout: %v", err)
			}
		})
	}
}

func TestCloneRunnerRepositoryUsesLocalGitConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("git clone integration")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	checkout(t, source)
	gitConfig := filepath.Join(root, "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(root, "absent-system-config"))
	runDoctorWorkflowSourceGit(t, source, "config", "--file", gitConfig, "url."+filepath.ToSlash(source)+".insteadOf", "https://github.com/acme/orders.git")
	selected := globalconfig.Project{Workdir: filepath.Join(root, "checkout")}
	for range 2 {
		if err := prepareRunnerCheckout(t.Context(), selected, "https://github.com/acme/orders.git"); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(filepath.Join(selected.Workdir, "WORKFLOW.md"))
	if err != nil || !strings.Contains(string(body), "Work the issue") {
		t.Fatalf("local git checkout: %q %v", body, err)
	}
}
