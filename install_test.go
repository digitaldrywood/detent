package symphony_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/symphony/internal/config/global"
)

func TestInstallScriptInstallsBinaryAndRefusesExistingLock(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source-symphony")
	if err := os.WriteFile(source, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	lockPath := filepath.Join(stateDir, "install.lock")
	env := append(os.Environ(),
		"HOME="+tmp,
		"SYMPHONY_INSTALL_SOURCE="+source,
		"SYMPHONY_INSTALL_DIR="+installDir,
		"SYMPHONY_STATE_DIR="+stateDir,
		"SYMPHONY_INSTALL_LOCK="+lockPath,
	)

	first := runInstall(t, root, env)
	if first.err != nil {
		t.Fatalf("first install error = %v\nstdout:\n%s\nstderr:\n%s", first.err, first.stdout, first.stderr)
	}
	if _, err := os.Stat(filepath.Join(installDir, "symphony")); err != nil {
		t.Fatalf("installed binary stat error = %v", err)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lock) error = %v", err)
	}
	if !strings.Contains(string(lock), filepath.Join(installDir, "symphony")) {
		t.Fatalf("lock file = %q, want installed binary path", string(lock))
	}

	second := runInstall(t, root, env)
	if second.err == nil {
		t.Fatal("second install error = nil, want lock failure")
	}
	if !strings.Contains(second.stderr, "already installed") {
		t.Fatalf("second install stderr = %q, want already installed", second.stderr)
	}
}

func TestFreshInstallBootsOnboardingWizardAndRunsSubcommands(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"SYMPHONY_INSTALL_DIR="+installDir,
		"SYMPHONY_STATE_DIR="+stateDir,
		"SYMPHONY_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
	)

	install := runInstallWithTimeout(t, root, env, time.Minute)
	if install.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", install.err, install.stdout, install.stderr)
	}

	binary := filepath.Join(installDir, "symphony")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("installed binary stat error = %v", err)
	}
	assertInstalledVersionMetadata(t, binary, root, env)

	home := filepath.Join(tmp, "home")
	workdir := filepath.Join(tmp, "fresh")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll(workdir) error = %v", err)
	}
	serverEnv := append(os.Environ(),
		"HOME="+home,
		"SYMPHONY_HOME="+filepath.Join(home, ".symphony"),
	)
	port := reservePort(t)
	stop := startSymphonyServer(t, binary, workdir, serverEnv, port)
	waitForOnboardingHealth(t, port)
	onboarding := readURL(t, fmt.Sprintf("http://127.0.0.1:%d/onboarding", port))
	if !strings.Contains(onboarding, "Symphony onboarding") {
		t.Fatalf("onboarding page missing wizard heading:\n%s", onboarding)
	}
	css := readURL(t, fmt.Sprintf("http://127.0.0.1:%d/static/css/output.css", port))
	if !strings.Contains(css, "tailwindcss") {
		t.Fatalf("static CSS missing Tailwind marker:\n%s", css)
	}
	stop()

	runInstalledSubcommands(t, binary, tmp, serverEnv)
}

type installRun struct {
	stdout string
	stderr string
	err    error
}

func runInstall(t *testing.T, root string, env []string) installRun {
	t.Helper()

	return runInstallWithTimeout(t, root, env, 10*time.Second)
}

func runInstallWithTimeout(t *testing.T, root string, env []string, timeout time.Duration) installRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "install.sh")
	cmd.Dir = root
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return installRun{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func reservePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	return addr.Port
}

func startSymphonyServer(t *testing.T, binary string, workdir string, env []string, port int) func() {
	t.Helper()

	cmd := exec.Command(binary, "--host", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Dir = workdir
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var once sync.Once
	stop := func() {
		t.Helper()

		once.Do(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("symphony shutdown error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
				}
			case <-time.After(2 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				t.Fatalf("timed out waiting for symphony shutdown\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
		})
	}
	t.Cleanup(stop)

	return stop
}

func waitForOnboardingHealth(t *testing.T, port int) {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := tryReadURL(url)
		if err == nil && strings.Contains(body, `"mode":"onboarding"`) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("health endpoint did not report onboarding mode at %s", url)
}

func readURL(t *testing.T, url string) string {
	t.Helper()

	body, err := tryReadURL(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	return body
}

func tryReadURL(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status = %d body = %s", resp.StatusCode, raw)
	}
	return string(raw), nil
}

func runInstalledSubcommands(t *testing.T, binary string, root string, env []string) {
	t.Helper()

	workdir := filepath.Join(root, "subcommands")
	projectDir := filepath.Join(workdir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(projectDir) error = %v", err)
	}
	workflowPath := filepath.Join(projectDir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(gateWorkflowContent()), 0o600); err != nil {
		t.Fatalf("WriteFile(workflow) error = %v", err)
	}
	configPath := filepath.Join(workdir, "global.yaml")

	runSymphonyCommand(t, binary, workdir, env, "--config", configPath, "init")
	runSymphonyCommand(t, binary, workdir, env,
		"--config", configPath,
		"add-project",
		"--id", "symphony",
		"--workflow", workflowPath,
		"--workdir", projectDir,
		"--weight", "2",
		"--priority", "4",
	)
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.ID != "symphony" || project.Workflow != workflowPath || project.Workdir != projectDir {
			t.Fatalf("project = %#v, want installed symphony project", project)
		}
		if project.Weight != 2 || project.Priority != 4 {
			t.Fatalf("project scheduling = weight %d priority %d, want 2/4", project.Weight, project.Priority)
		}
	})

	runSymphonyCommand(t, binary, workdir, env, "--config", configPath, "pause", "symphony")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if !project.Paused {
			t.Fatal("project Paused = false, want true")
		}
	})

	runSymphonyCommand(t, binary, workdir, env, "--config", configPath, "unpause", "symphony")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.Paused {
			t.Fatal("project Paused = true, want false")
		}
	})

	runSymphonyCommand(t, binary, workdir, env, "--config", configPath, "promote", "symphony", "--priority", "1")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.Priority != 1 {
			t.Fatalf("project Priority = %d, want 1", project.Priority)
		}
	})

	runSymphonyCommand(t, binary, workdir, env, "--config", configPath, "remove-project", "symphony")
	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want none", cfg.Projects)
	}
}

func runSymphonyCommand(t *testing.T, binary string, workdir string, env []string, args ...string) {
	t.Helper()

	stdout, stderr, err := runSymphonyCommandOutput(t, binary, workdir, env, args...)
	if err != nil {
		t.Fatalf("symphony %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout, stderr)
	}
}

func runSymphonyCommandOutput(t *testing.T, binary string, workdir string, env []string, args ...string) (string, string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workdir
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func assertInstalledVersionMetadata(t *testing.T, binary string, root string, env []string) {
	t.Helper()

	wantVersion, ok := gitOutput(t, root, "describe", "--tags", "--always")
	if !ok {
		t.Skip("git metadata unavailable")
	}
	wantCommit, ok := gitOutput(t, root, "rev-parse", "--short", "HEAD")
	if !ok {
		t.Skip("git metadata unavailable")
	}

	stdout, stderr, err := runSymphonyCommandOutput(t, binary, root, env, "--version")
	if err != nil {
		t.Fatalf("symphony --version error = %v\nstderr:\n%s", err, stderr)
	}
	if got, want := stdout, wantVersion+"\n"; got != want {
		t.Fatalf("symphony --version = %q, want %q", got, want)
	}

	stdout, stderr, err = runSymphonyCommandOutput(t, binary, root, env, "version")
	if err != nil {
		t.Fatalf("symphony version error = %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"version: " + wantVersion,
		"commit: " + wantCommit,
		"go version: " + runtime.Version(),
		"os/arch: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("symphony version output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "build date: unknown") {
		t.Fatalf("symphony version output kept unknown build date:\n%s", stdout)
	}
}

func gitOutput(t *testing.T, root string, args ...string) (string, bool) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func assertInstalledProject(t *testing.T, configPath string, check func(globalconfig.Project)) {
	t.Helper()

	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects length = %d, want 1", len(cfg.Projects))
	}
	check(cfg.Projects[0])
}

func gateWorkflowContent() string {
	return `---
tracker:
  kind: memory
---
Gate prompt.
`
}
