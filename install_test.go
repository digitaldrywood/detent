//go:build !windows

package detent_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestInstallScriptInstallsBinaryAndRefusesExistingLock(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source-detent")
	if err := os.WriteFile(source, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	lockPath := filepath.Join(stateDir, "install.lock")
	env := append(os.Environ(),
		"HOME="+tmp,
		"DETENT_INSTALL_SOURCE="+source,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+lockPath,
	)

	first := runInstall(t, root, env)
	if first.err != nil {
		t.Fatalf("first install error = %v\nstdout:\n%s\nstderr:\n%s", first.err, first.stdout, first.stderr)
	}
	if _, err := os.Stat(filepath.Join(installDir, "detent")); err != nil {
		t.Fatalf("installed binary stat error = %v", err)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lock) error = %v", err)
	}
	if !strings.Contains(string(lock), filepath.Join(installDir, "detent")) {
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

func TestInstallScriptDetectsSupportedTargets(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source-detent")
	if err := os.WriteFile(source, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	tests := []struct {
		name   string
		unameS string
		unameM string
		want   string
	}{
		{name: "darwin arm64", unameS: "Darwin", unameM: "arm64", want: "Detected target darwin/arm64"},
		{name: "linux amd64", unameS: "Linux", unameM: "x86_64", want: "Detected target linux/amd64"},
		{name: "linux arm64", unameS: "Linux", unameM: "aarch64", want: "Detected target linux/arm64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			caseDir := filepath.Join(tmp, strings.ReplaceAll(tt.name, " ", "-"))
			installDir := filepath.Join(caseDir, "bin")
			stateDir := filepath.Join(caseDir, "state")
			env := append(os.Environ(),
				"HOME="+caseDir,
				"DETENT_INSTALL_SOURCE="+source,
				"DETENT_INSTALL_DIR="+installDir,
				"DETENT_STATE_DIR="+stateDir,
				"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
				"DETENT_INSTALL_TEST_UNAME_S="+tt.unameS,
				"DETENT_INSTALL_TEST_UNAME_M="+tt.unameM,
			)

			result := runInstall(t, root, env)
			if result.err != nil {
				t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
			}
			if !strings.Contains(result.stdout, tt.want) {
				t.Fatalf("install stdout = %q, want %q", result.stdout, tt.want)
			}
		})
	}
}

func TestInstallScriptInstallsReleaseArchive(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	archiveName := "detent_1.2.3_linux_amd64.tar.gz"
	archive := detentArchive(t, "#!/usr/bin/env sh\nprintf 'release-ok\\n'\n")
	server := newInstallReleaseServer("v1.2.3", archiveName, archive, archiveChecksum(archive))
	defer server.Close()

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"DETENT_GITHUB_API_BASE="+server.URL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_INSTALL_MODE=release",
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
		"DETENT_INSTALL_TEST_UNAME_S=Linux",
		"DETENT_INSTALL_TEST_UNAME_M=x86_64",
	)

	result := runInstall(t, root, env)
	if result.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	binary := filepath.Join(installDir, "detent")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("installed binary stat error = %v", err)
	}
	stdout, stderr, err := runDetentCommandOutput(t, binary, root, env)
	if err != nil {
		t.Fatalf("installed release binary error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout != "release-ok\n" {
		t.Fatalf("installed release binary stdout = %q, want release-ok", stdout)
	}
}

func TestInstallScriptAbortsOnChecksumMismatch(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	archiveName := "detent_1.2.3_linux_amd64.tar.gz"
	archive := detentArchive(t, "#!/usr/bin/env sh\nexit 0\n")
	server := newInstallReleaseServer("v1.2.3", archiveName, archive, strings.Repeat("0", 64))
	defer server.Close()

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"DETENT_GITHUB_API_BASE="+server.URL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_INSTALL_MODE=release",
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
		"DETENT_INSTALL_TEST_UNAME_S=Linux",
		"DETENT_INSTALL_TEST_UNAME_M=x86_64",
	)

	result := runInstall(t, root, env)
	if result.err == nil {
		t.Fatal("install error = nil, want checksum failure")
	}
	if !strings.Contains(result.stderr, "Checksum mismatch for "+archiveName) {
		t.Fatalf("install stderr = %q, want checksum mismatch for %s", result.stderr, archiveName)
	}
	if _, err := os.Stat(filepath.Join(installDir, "detent")); !os.IsNotExist(err) {
		t.Fatalf("installed binary stat error = %v, want not exist", err)
	}
}

func TestInstallScriptFallsBackToGoInstallWhenReleaseAssetMissing(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	tmp := t.TempDir()
	fakeBin := filepath.Join(tmp, "fakebin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(fakebin) error = %v", err)
	}
	fakeGo := filepath.Join(fakeBin, "go")
	fakeGoScript := `#!/usr/bin/env sh
if [ "$1" = install ]; then
mkdir -p "$GOBIN"
cat > "$GOBIN/detent" <<'EOF'
#!/usr/bin/env sh
printf 'go-install-ok\n'
EOF
chmod 755 "$GOBIN/detent"
exit 0
fi
exit 1
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("WriteFile(fake go) error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DETENT_GITHUB_API_BASE="+server.URL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_INSTALL_MODE=release",
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
		"DETENT_INSTALL_TEST_UNAME_S=Linux",
		"DETENT_INSTALL_TEST_UNAME_M=x86_64",
	)

	result := runInstall(t, root, env)
	if result.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "No Detent release asset found") {
		t.Fatalf("install stderr = %q, want release asset fallback", result.stderr)
	}

	binary := filepath.Join(installDir, "detent")
	stdout, stderr, err := runDetentCommandOutput(t, binary, root, env)
	if err != nil {
		t.Fatalf("installed fallback binary error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if stdout != "go-install-ok\n" {
		t.Fatalf("installed fallback binary stdout = %q, want go-install-ok", stdout)
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
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
	)

	install := runInstallWithTimeout(t, root, env, time.Minute)
	if install.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", install.err, install.stdout, install.stderr)
	}

	binary := filepath.Join(installDir, "detent")
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
		"DETENT_HOME="+filepath.Join(home, ".detent"),
	)
	port := reservePort(t)
	stop := startDetentServer(t, binary, workdir, serverEnv, port)
	waitForOnboardingHealth(t, port)
	onboarding := readURL(t, fmt.Sprintf("http://127.0.0.1:%d/onboarding", port))
	if !strings.Contains(onboarding, "Detent onboarding") {
		t.Fatalf("onboarding page missing wizard heading:\n%s", onboarding)
	}
	css := readURL(t, fmt.Sprintf("http://127.0.0.1:%d/static/css/output.css", port))
	if !strings.Contains(css, "tailwindcss") {
		t.Fatalf("static CSS missing Tailwind marker:\n%s", css)
	}
	stop()

	runInstalledSubcommands(t, binary, tmp, serverEnv)
}

func detentArchive(t *testing.T, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	header := &tar.Header{
		Name: "detent",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	return buf.Bytes()
}

func archiveChecksum(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum)
}

func newInstallReleaseServer(tag string, archiveName string, archive []byte, checksum string) *httptest.Server {
	version := strings.TrimPrefix(tag, "v")
	checksumName := "detent_" + version + "_checksums.txt"

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case "/" + tag + "/" + archiveName:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		case "/" + tag + "/" + checksumName:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s  %s\n", checksum, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
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

	cmd := exec.CommandContext(ctx, "sh", "install.sh")
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

func startDetentServer(t *testing.T, binary string, workdir string, env []string, port int) func() {
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
					t.Fatalf("detent shutdown error = %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
				}
			case <-time.After(2 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				t.Fatalf("timed out waiting for detent shutdown\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
			}
		})
	}
	t.Cleanup(stop)

	return stop
}

func waitForOnboardingHealth(t *testing.T, port int) {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()

	for {
		body, err := tryReadURL(url)
		if err == nil && strings.Contains(body, `"mode":"onboarding"`) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("health endpoint did not report onboarding mode at %s", url)
		case <-tick.C:
		}
	}
}

func readURL(t *testing.T, url string) string {
	t.Helper()

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()

	var lastErr error
	for {
		body, err := tryReadURL(url)
		if err == nil {
			return body
		}
		lastErr = err
		select {
		case <-deadline.C:
			t.Fatalf("GET %s error = %v", url, lastErr)
		case <-tick.C:
		}
	}
}

func tryReadURL(url string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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

	runDetentCommand(t, binary, workdir, env, "--config", configPath, "init")
	runDetentCommand(t, binary, workdir, env,
		"--config", configPath,
		"add-project",
		"--id", "detent",
		"--workflow", workflowPath,
		"--workdir", projectDir,
		"--weight", "2",
		"--priority", "4",
	)
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.ID != "detent" || project.Workflow != workflowPath || project.Workdir != projectDir {
			t.Fatalf("project = %#v, want installed detent project", project)
		}
		if project.Weight != 2 || project.Priority != 4 {
			t.Fatalf("project scheduling = weight %d priority %d, want 2/4", project.Weight, project.Priority)
		}
	})

	runDetentCommand(t, binary, workdir, env, "--config", configPath, "pause", "detent")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if !project.Paused {
			t.Fatal("project Paused = false, want true")
		}
	})

	runDetentCommand(t, binary, workdir, env, "--config", configPath, "unpause", "detent")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.Paused {
			t.Fatal("project Paused = true, want false")
		}
	})

	runDetentCommand(t, binary, workdir, env, "--config", configPath, "promote", "detent", "--priority", "1")
	assertInstalledProject(t, configPath, func(project globalconfig.Project) {
		if project.Priority != 1 {
			t.Fatalf("project Priority = %d, want 1", project.Priority)
		}
	})

	runDetentCommand(t, binary, workdir, env, "--config", configPath, "remove-project", "detent")
	cfg, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want none", cfg.Projects)
	}
}

func runDetentCommand(t *testing.T, binary string, workdir string, env []string, args ...string) {
	t.Helper()

	stdout, stderr, err := runDetentCommandOutput(t, binary, workdir, env, args...)
	if err != nil {
		t.Fatalf("detent %v error = %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout, stderr)
	}
}

func runDetentCommandOutput(t *testing.T, binary string, workdir string, env []string, args ...string) (string, string, error) {
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

	stdout, stderr, err := runDetentCommandOutput(t, binary, root, env, "--version")
	if err != nil {
		t.Fatalf("detent --version error = %v\nstderr:\n%s", err, stderr)
	}
	if got, want := stdout, wantVersion+"\n"; got != want {
		t.Fatalf("detent --version = %q, want %q", got, want)
	}

	stdout, stderr, err = runDetentCommandOutput(t, binary, root, env, "version")
	if err != nil {
		t.Fatalf("detent version error = %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{
		"version: " + wantVersion,
		"commit: " + wantCommit,
		"go version: " + runtime.Version(),
		"os/arch: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("detent version output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "build date: unknown") {
		t.Fatalf("detent version output kept unknown build date:\n%s", stdout)
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
