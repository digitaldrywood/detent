package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
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
			if !test.service && !strings.Contains(output, "detent --config") {
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
	runDoctorWorkflowSourceGit(t, source, "config", "uploadpack.allowFilter", "true")
	archive := make([]byte, 256*1024)
	if _, err := rand.Read(archive); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "archive.bin"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	runDoctorWorkflowSourceGit(t, source, "add", "archive.bin")
	runDoctorWorkflowSourceGit(t, source, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "archive")
	historic := strings.TrimSpace(runDoctorWorkflowSourceGit(t, source, "rev-parse", "HEAD"))
	runDoctorWorkflowSourceGit(t, source, "rm", "archive.bin")
	runDoctorWorkflowSourceGit(t, source, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "remove archive")
	gitConfig := filepath.Join(root, "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(root, "absent-system-config"))
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var transferred atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.ServeHTTP(&runnerCheckoutTransferWriter{ResponseWriter: w, transferred: &transferred}, r)
	}))
	defer server.Close()
	runDoctorWorkflowSourceGit(t, source, "config", "--file", gitConfig, "url."+server.URL+"/source.insteadOf", "https://github.com/example-org/example-project.git")
	full := exec.CommandContext(t.Context(), "git", "clone", "--", "https://github.com/example-org/example-project.git", filepath.Join(root, "full-clone"))
	procgroup.Configure(t.Context(), full)
	if output, err := full.CombinedOutput(); err != nil {
		t.Fatalf("full clone: %v: %s", err, output)
	}
	fullBytes := transferred.Swap(0)
	if fullBytes < int64(len(archive)) {
		t.Fatalf("full clone transferred %d bytes, want at least the %d-byte history blob", fullBytes, len(archive))
	}
	selected := globalconfig.Project{Workdir: filepath.Join(root, "checkout")}
	clone := func(ctx context.Context, cloneURL, staging string) error {
		return cloneRunnerRepositoryWithCommandContext(ctx, cloneURL, staging, func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
			if timeout != 2*time.Minute {
				t.Fatalf("clone timeout = %s, want two minutes", timeout)
			}
			return context.WithCancel(ctx)
		})
	}
	for range 2 {
		if err := prepareRunnerCheckoutWithClone(t.Context(), selected, "https://github.com/example-org/example-project.git", clone); err != nil {
			t.Fatal(err)
		}
	}
	if partialBytes := transferred.Load(); partialBytes <= 0 || partialBytes >= fullBytes {
		t.Fatalf("partial clone transferred %d bytes, want fewer than the full clone's %d bytes", partialBytes, fullBytes)
	}
	missing := runDoctorWorkflowSourceGit(t, selected.Workdir, "rev-list", "--objects", "--all", "--missing=print")
	if !strings.Contains(missing, "\n?") {
		t.Fatalf("clone downloaded obsolete history blobs: %s", missing)
	}
	fresh := filepath.Join(root, "exact-workspace")
	runDoctorWorkflowSourceGit(t, selected.Workdir, "worktree", "add", "--detach", fresh, historic)
	if body, err := os.ReadFile(filepath.Join(fresh, "archive.bin")); err != nil || len(body) != len(archive) {
		t.Fatalf("exact historic workspace could not retrieve missing blob: %d bytes, %v", len(body), err)
	}
	body, err := os.ReadFile(filepath.Join(selected.Workdir, "WORKFLOW.md"))
	if err != nil || !strings.Contains(string(body), "Work the issue") {
		t.Fatalf("local git checkout: %q %v", body, err)
	}
}

func TestCloneRunnerRepositoryFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX git fixture")
	}
	for _, test := range []struct {
		name, stderr, fix string
		timeout           bool
		oversized         bool
	}{
		{name: "authentication", stderr: "fatal: Authentication failed for https://user:password@github.com/example-org/project.git", fix: "gh auth login"},
		{name: "not found", stderr: "fatal: repository not found", fix: "Verify the repository binding"},
		{name: "network", stderr: "fatal: Could not resolve host: github.com", fix: "Check repository connectivity"},
		{name: "timeout", stderr: "transfer stalled", timeout: true, fix: "Repository checkout timed out"},
		{name: "excessive diagnostics", stderr: "fatal: Authentication failed for https://user:password@github.com/example-org/project.git", fix: "gh auth login", oversized: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			script := "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\nexit 128\n"
			if test.oversized {
				script = "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\ni=0\nwhile [ \"$i\" -lt 1024 ]; do printf '%01024d' 0 >&2; i=$((i+1)); done\nprintf discarded-suffix >&2\nexit 128\n"
			}
			if test.timeout {
				script = "#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_STDERR\" >&2\nprintf ready > \"$CHECKOUT_STARTED\"\nwhile :; do :; done\n"
			}
			if err := os.WriteFile(filepath.Join(root, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root)
			t.Setenv("CHECKOUT_STDERR", test.stderr)
			started := filepath.Join(root, "started")
			t.Setenv("CHECKOUT_STARTED", started)
			contexts := newControlledRuntimeCommandContexts()
			contexts.AllowCommand()
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- cloneRunnerRepositoryWithCommandContext(t.Context(), "https://github.com/example-org/project.git", root, contexts.New)
			}()
			<-contexts.ready
			t.Cleanup(func() {
				contexts.Expire()
				<-done
			})
			if test.timeout {
				testenv.WaitUntil(t, t.Context(), func() error {
					_, err := os.Stat(started)
					return err
				})
				contexts.Expire()
			}
			err := <-done
			var checkoutErr *runnerCheckoutError
			if !errors.As(err, &checkoutErr) || !strings.Contains(checkoutErr.fix, test.fix) {
				t.Fatalf("checkout failure: %v", err)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("lost git exit status: %v", err)
			}
			if errors.Is(err, context.DeadlineExceeded) != test.timeout {
				t.Fatalf("deadline cause: %v", err)
			}
			if !test.timeout && exitErr.ExitCode() != 128 {
				t.Fatalf("exit status = %d", exitErr.ExitCode())
			}
			if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "user:") {
				t.Fatalf("credentials leaked: %v", err)
			}
			checks := runnerauth.LocalChecks{Checkout: "failed", Doctor: "pending", Provider: "pending", CheckoutMessage: checkoutErr.guidance(), CheckoutFix: checkoutErr.fix}
			if err := checks.Validate(); err != nil {
				t.Fatalf("Hub rejected checkout guidance: %v", err)
			}
			if test.timeout && !strings.Contains(err.Error(), "transfer stalled") {
				t.Fatalf("lost stderr: %v", err)
			}
			if test.oversized {
				if len(err.Error()) > 9*1024 || !strings.Contains(err.Error(), "[stderr truncated]") || strings.Contains(err.Error(), "discarded-suffix") {
					t.Fatalf("stderr was not bounded with a truncation marker: %d bytes", len(err.Error()))
				}
				if !strings.Contains(err.Error(), "Authentication failed") {
					t.Fatal("lost retained stderr")
				}
			} else if strings.Contains(err.Error(), "[stderr truncated]") {
				t.Fatal("short stderr was marked truncated")
			}
		})
	}
}

type runnerCheckoutTransferWriter struct {
	http.ResponseWriter
	transferred *atomic.Int64
}

func (w *runnerCheckoutTransferWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.transferred.Add(int64(n))
	return n, err
}

func TestRunnerCheckoutRefreshPreservesEdits(t *testing.T) {
	t.Parallel()
	for _, dirty := range []bool{false, true} {
		t.Run(fmt.Sprintf("dirty=%t", dirty), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source, target := filepath.Join(root, "source"), filepath.Join(root, "checkout")
			checkout(t, source)
			runDoctorWorkflowSourceGit(t, root, "clone", source, target)
			if err := os.WriteFile(filepath.Join(source, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: hub_native\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runDoctorWorkflowSourceGit(t, source, "add", "detent.yaml")
			runDoctorWorkflowSourceGit(t, source, "-c", "user.name=Test runner", "-c", "user.email=runner@example.test", "-c", "commit.gpgsign=false", "commit", "-m", "add configuration")
			if dirty {
				if err := os.WriteFile(filepath.Join(target, "WORKFLOW.md"), []byte("Local edits\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := refreshRunnerCheckout(t.Context(), globalconfig.Project{Workdir: target}); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(filepath.Join(target, "detent.yaml"))
			if dirty {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("dirty checkout was updated: %v", err)
				}
				raw, err := os.ReadFile(filepath.Join(target, "WORKFLOW.md"))
				if err != nil || string(raw) != "Local edits\n" {
					t.Fatalf("lost local edits: %q %v", raw, err)
				}
			} else if err != nil {
				t.Fatalf("configuration commit not picked up: %v", err)
			}
		})
	}
}

func TestRunnerStarterFiles(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, workflow, config string }{
		{name: "empty"},
		{name: "existing instructions", workflow: "Existing project instructions.\n"},
		{name: "existing configuration", config: "schema: 1\ntracker:\n  kind: hub_native\n"},
		{name: "legacy definition", workflow: "---\ntracker:\n  kind: hub_native\n---\nExisting instructions.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for file, content := range map[string]string{"WORKFLOW.md": test.workflow, "detent.yaml": test.config} {
				if content != "" {
					if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			external := filepath.Join(t.TempDir(), "WORKFLOW.md")
			selected := globalconfig.Project{Workdir: root, Workflow: external}
			for range 2 {
				if err := createRunnerStarterFiles(selected, "example-org/example-project"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(external); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrote outside checkout: %v", err)
			}
			for file, content := range map[string]string{"WORKFLOW.md": test.workflow, "detent.yaml": test.config} {
				if content == "" {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(root, file))
				if err != nil || string(raw) != content {
					t.Fatalf("overwrote %s: %q %v", file, raw, err)
				}
			}
			workflow, err := workflowconfig.LoadWorkflow(filepath.Join(root, "WORKFLOW.md"))
			if err != nil || workflow.Prompt == "" {
				t.Fatalf("starter definition cannot load (prompt bytes %d): %v", len(workflow.Prompt), err)
			}
		})
	}
}
