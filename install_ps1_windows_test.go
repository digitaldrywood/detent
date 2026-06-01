//go:build windows

package detent_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPowerShellInstallScriptInstallsReleaseArchive(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	archiveName := "detent_1.2.3_windows_amd64.zip"
	archive := detentWindowsArchive(t, "release-ok")
	server := newPowerShellInstallReleaseServer("v1.2.3", archiveName, archive, windowsArchiveChecksum(archive))
	t.Cleanup(server.Close)

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "bin")
	env := append(os.Environ(),
		"DETENT_GITHUB_API_BASE="+server.URL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_INSTALL_MODE=release",
		"DETENT_INSTALL_SKIP_PATH=1",
		"DETENT_INSTALL_TEST_ARCH=amd64",
	)

	result := runPowerShellInstall(t, root, env)
	if result.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	binary := filepath.Join(installDir, "detent.exe")
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("ReadFile(installed binary) error = %v", err)
	}
	if string(raw) != "release-ok" {
		t.Fatalf("installed binary = %q, want release-ok", raw)
	}
	if !strings.Contains(result.stdout, "Verified checksum for "+archiveName) {
		t.Fatalf("install stdout = %q, want checksum verification", result.stdout)
	}
}

func detentWindowsArchive(t *testing.T, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writer, err := zw.Create("detent.exe")
	if err != nil {
		t.Fatalf("Create(detent.exe) error = %v", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("Write(detent.exe) error = %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	return buf.Bytes()
}

func windowsArchiveChecksum(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("%x", sum)
}

func newPowerShellInstallReleaseServer(tag string, archiveName string, archive []byte, checksum string) *httptest.Server {
	version := strings.TrimPrefix(tag, "v")
	checksumName := "detent_" + version + "_checksums.txt"

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case "/" + tag + "/" + archiveName:
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(archive)
		case "/" + tag + "/" + checksumName:
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s  %s\n", checksum, archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
}

type powerShellInstallRun struct {
	stdout string
	stderr string
	err    error
}

func runPowerShellInstall(t *testing.T, root string, env []string) powerShellInstallRun {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", "install.ps1")
	cmd.Dir = root
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return powerShellInstallRun{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}
