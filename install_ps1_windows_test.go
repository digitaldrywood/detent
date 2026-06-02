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
	stateDir := filepath.Join(tmp, "state")
	env := powerShellInstallEnv(
		"DETENT_GITHUB_API_BASE="+server.URL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
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
	lock, err := os.ReadFile(filepath.Join(stateDir, "install.lock"))
	if err != nil {
		t.Fatalf("ReadFile(install lock) error = %v", err)
	}
	if !strings.Contains(string(lock), binary) {
		t.Fatalf("install lock = %q, want binary path %q", lock, binary)
	}
}

func TestPowerShellInstallScriptMapsX86ProcessToOSArchitecture(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tests := []struct {
		name          string
		osArch        string
		processArch   string
		wow64Arch     string
		processorArch string
		cimArch       string
		cimOSArch     string
		runtimeSkip   string
		assetArch     string
		archiveOut    string
	}{
		{name: "amd64 os", osArch: "AMD64", processArch: "X86", wow64Arch: "AMD64", processorArch: "x86", assetArch: "amd64", archiveOut: "release-amd64"},
		{name: "arm64 os", osArch: "ARM64", processArch: "X86", wow64Arch: "ARM64", processorArch: "x86", assetArch: "arm64", archiveOut: "release-arm64"},
		{name: "generic 64 bit os defers to arm64 process", osArch: "64-bit", processArch: "ARM64", processorArch: "ARM64", cimArch: "0", assetArch: "arm64", archiveOut: "release-arm64"},
		{name: "runtime unavailable arm64 env before generic cim os", processorArch: "ARM64", cimArch: "0", cimOSArch: "64-bit", runtimeSkip: "1", assetArch: "arm64", archiveOut: "release-arm64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			archiveName := "detent_1.2.3_windows_" + tt.assetArch + ".zip"
			archive := detentWindowsArchive(t, tt.archiveOut)
			server := newPowerShellInstallReleaseServer("v1.2.3", archiveName, archive, windowsArchiveChecksum(archive))
			t.Cleanup(server.Close)

			tmp := t.TempDir()
			installDir := filepath.Join(tmp, "bin")
			stateDir := filepath.Join(tmp, "state")
			fakeBin := filepath.Join(tmp, "fakebin")
			if err := os.MkdirAll(fakeBin, 0o755); err != nil {
				t.Fatalf("MkdirAll(fakebin) error = %v", err)
			}
			fakeGo := filepath.Join(fakeBin, "go.cmd")
			if err := os.WriteFile(fakeGo, []byte("@echo off\r\nexit /b 1\r\n"), 0o755); err != nil {
				t.Fatalf("WriteFile(fake go) error = %v", err)
			}

			env := powerShellInstallEnv(
				"DETENT_GITHUB_API_BASE="+server.URL,
				"DETENT_RELEASE_DOWNLOAD_BASE="+server.URL,
				"DETENT_INSTALL_DIR="+installDir,
				"DETENT_STATE_DIR="+stateDir,
				"DETENT_INSTALL_MODE=release",
				"DETENT_INSTALL_SKIP_PATH=1",
				"DETENT_INSTALL_TEST_OS_ARCH="+tt.osArch,
				"DETENT_INSTALL_TEST_PROCESS_ARCH="+tt.processArch,
				"DETENT_INSTALL_TEST_PROCESSOR_ARCHITECTURE="+tt.processorArch,
				"DETENT_INSTALL_TEST_PROCESSOR_ARCHITEW6432="+tt.wow64Arch,
				"PROCESSOR_ARCHITECTURE="+tt.processorArch,
				"PROCESSOR_ARCHITEW6432="+tt.wow64Arch,
				"DETENT_INSTALL_TEST_CIM_PROCESSOR_ARCH="+tt.cimArch,
				"DETENT_INSTALL_TEST_CIM_OS_ARCH="+tt.cimOSArch,
				"DETENT_INSTALL_TEST_OS_ARCH_UNAVAILABLE="+tt.runtimeSkip,
				"PATH="+fakeBin+";"+os.Getenv("PATH"),
			)

			result := runPowerShellInstall(t, root, env)
			if result.err != nil {
				t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
			}
			if !strings.Contains(result.stdout, "Detected target windows/"+tt.assetArch) {
				t.Fatalf("install stdout = %q, want target windows/%s", result.stdout, tt.assetArch)
			}

			binary := filepath.Join(installDir, "detent.exe")
			raw, err := os.ReadFile(binary)
			if err != nil {
				t.Fatalf("ReadFile(installed binary) error = %v", err)
			}
			if string(raw) != tt.archiveOut {
				t.Fatalf("installed binary = %q, want %s", raw, tt.archiveOut)
			}
		})
	}
}

func powerShellInstallEnv(overrides ...string) []string {
	env := os.Environ()
	for _, override := range overrides {
		key, _, ok := strings.Cut(override, "=")
		if !ok {
			continue
		}

		filtered := make([]string, 0, len(env)+1)
		for _, entry := range env {
			existingKey, _, ok := strings.Cut(entry, "=")
			if ok && strings.EqualFold(existingKey, key) {
				continue
			}
			filtered = append(filtered, entry)
		}
		env = append(filtered, override)
	}
	return env
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
