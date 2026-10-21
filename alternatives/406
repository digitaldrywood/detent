//go:build !windows

package detent_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestInstallScriptExistingInstall(t *testing.T) {
	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	server := newInstallReleaseServer("v1.2.3", "unused.tar.gz", nil, "")
	t.Cleanup(server.Close)

	tests := []struct {
		name          string
		version       string
		required      string
		lock          string
		removeBinary  bool
		versionFails  bool
		homebrew      bool
		nonExecutable bool
		wantError     string
	}{
		{name: "completed current install", version: "v1.2.3"},
		{name: "current latest release", version: "v1.2.3", required: "latest"},
		{name: "latest release unavailable", version: "v1.2.3", required: "unavailable", wantError: "Retry the enrollment command when the release service is reachable"},
		{name: "completed newer install", version: "v1.10.0"},
		{name: "build metadata", version: "v1.2.3+build.1"},
		{name: "other install method", version: "v1.2.3", lock: "absent"},
		{name: "current binary after install method changed", version: "v1.2.3", lock: "stale"},
		{name: "concurrent install", version: "v1.2.3", lock: "empty", wantError: "Wait for it to finish"},
		{name: "incomplete install record", version: "v1.2.3", lock: "partial", wantError: "Wait for it to finish"},
		{name: "missing recorded binary", version: "v1.2.3", removeBinary: true, wantError: "remove the completed install record with rm"},
		{name: "older install", version: "v1.2.2", wantError: "update --yes --from-release"},
		{name: "older prerelease", version: "v1.2.3-rc.1", wantError: "update --yes --from-release"},
		{name: "newer prerelease", version: "v1.3.0-rc.1"},
		{name: "numeric prerelease ordering", version: "v1.2.3-rc.10", required: "v1.2.3-rc.2"},
		{name: "unknown installed version", version: "dev", wantError: "update --yes --from-release"},
		{name: "unreadable version", version: "", wantError: "Cannot read the version"},
		{name: "version command fails", version: "v1.2.3", versionFails: true, wantError: "Cannot read the version"},
		{name: "older external install", version: "v1.2.2", lock: "absent", wantError: "This switches installations managed by go install"},
		{name: "older Homebrew install", version: "v1.2.2", lock: "absent", homebrew: true, wantError: "brew upgrade digitaldrywood/tap/detent"},
		{name: "non-executable external file", version: "v1.2.3", lock: "absent", nonExecutable: true, wantError: "Move it aside with mv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmp := t.TempDir()
			source := filepath.Join(tmp, "source-detent")
			versionExit := 0
			if tt.versionFails {
				versionExit = 1
			}
			content := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --format ]; then\n  printf 'version: %%s\\n' '%s'\n  exit %d\nfi\nif [ \"$1\" = hub ]; then printf 'registered\\n'; exit 0; fi\nexit 1\n", tt.version, versionExit)
			if err := os.WriteFile(source, []byte(content), 0o755); err != nil {
				t.Fatal(err)
			}
			installDir := filepath.Join(tmp, "bin")
			if tt.homebrew {
				installDir = filepath.Join(tmp, "Cellar", "detent", "1.2.2", "bin")
			}
			stateDir := filepath.Join(tmp, "state")
			binary := filepath.Join(installDir, "detent")
			lockPath := filepath.Join(stateDir, "install.lock")
			required := tt.required
			if required == "" {
				required = "v1.2.3"
			}
			apiBase := server.URL
			if required == "latest" || required == "unavailable" {
				if required == "unavailable" {
					apiBase += "/unavailable"
				}
				required = ""
			}
			env := append(os.Environ(),
				"HOME="+tmp,
				"DETENT_VERSION="+required,
				"DETENT_GITHUB_API_BASE="+apiBase,
				"DETENT_INSTALL_SOURCE="+source,
				"DETENT_INSTALL_DIR="+installDir,
				"DETENT_STATE_DIR="+stateDir,
				"DETENT_INSTALL_LOCK="+lockPath,
			)
			first := runInstall(t, root, env)
			if first.err != nil {
				t.Fatalf("first install error = %v\nstdout:\n%s\nstderr:\n%s", first.err, first.stdout, first.stderr)
			}
			lock, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(lock), "binary="+binary+"\n") {
				t.Fatalf("lock file = %q, want installed binary path", lock)
			}
			switch tt.lock {
			case "stale":
				lock = []byte(strings.ReplaceAll(string(lock), binary, filepath.Join(tmp, "removed-detent")))
				if err := os.WriteFile(lockPath, lock, 0o644); err != nil {
					t.Fatal(err)
				}
			case "absent":
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
			case "empty", "partial":
				lock = nil
				if tt.lock == "partial" {
					lock = []byte("binary=" + binary + "\n")
				}
				if err := os.WriteFile(lockPath, lock, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.removeBinary {
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
			}
			if tt.nonExecutable {
				if err := os.Chmod(binary, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", `sh install.sh && export PATH="$DETENT_INSTALL_DIR:$PATH" && detent hub runner register --url https://hub.example.test --token test-token`)
			cmd.Dir = root
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			if tt.wantError != "" {
				if err == nil || !strings.Contains(string(output), tt.wantError) {
					t.Fatalf("repeat install error = %v, output = %s, want %q", err, output, tt.wantError)
				}
				if strings.Contains(string(output), "registered") {
					t.Fatalf("registration ran after a refused install: %s", output)
				}
			} else {
				if err != nil {
					t.Fatalf("repeat install error = %v, output = %s", err, output)
				}
				for _, want := range []string{"Using existing Detent at " + binary + " (version " + tt.version + ")", "registered\n"} {
					if !strings.Contains(string(output), want) {
						t.Fatalf("repeat install output = %s, want %q", output, want)
					}
				}
			}
			if tt.lock == "absent" {
				if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("external install acquired an installer record: %v", err)
				}
			} else {
				after, err := os.ReadFile(lockPath)
				if err != nil || !bytes.Equal(after, lock) {
					t.Fatalf("install record changed: %q, error = %v, want %q", after, err, lock)
				}
			}
			if !tt.removeBinary {
				after, err := os.ReadFile(binary)
				if err != nil || string(after) != content {
					t.Fatalf("existing binary changed: %q, error = %v", after, err)
				}
			}
		})
	}
}

func TestInstallScriptDetectsSupportedTargets(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

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
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

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
	if !strings.Contains(result.stdout, "Verified checksum for "+archiveName) {
		t.Fatalf("install stdout = %q, want checksum verification", result.stdout)
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

func TestInstallScriptAuthenticatesLatestReleaseRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	const token = "ci-token"
	archiveName := "detent_1.2.3_linux_amd64.tar.gz"
	archive := detentArchive(t, "#!/usr/bin/env sh\nprintf 'release-ok\\n'\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
				http.Error(w, "missing authorization", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
		case "/v1.2.3/" + archiveName:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		case "/v1.2.3/detent_1.2.3_checksums.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s  %s\n", archiveChecksum(archive), archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmp := t.TempDir()
	fakeBin := filepath.Join(tmp, "fakebin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(fakebin) error = %v", err)
	}
	fakeGo := filepath.Join(fakeBin, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/usr/bin/env sh\nprintf 'unexpected go install fallback\\n' >&2\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(fake go) error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GITHUB_TOKEN="+token,
		"DETENT_GITHUB_TOKEN=",
		"DETENT_VERSION=",
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
	if !strings.Contains(result.stdout, "Verified checksum for "+archiveName) {
		t.Fatalf("install stdout = %q, want checksum verification", result.stdout)
	}
}

func TestInstallScriptDoesNotForwardTokenAcrossLatestReleaseRedirect(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	const token = "ci-token"
	leaked := make(chan string, 1)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case leaked <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
	}))
	defer redirectTarget.Close()

	archiveName := "detent_1.2.3_linux_amd64.tar.gz"
	archive := detentArchive(t, "#!/usr/bin/env sh\nprintf 'release-ok\\n'\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest":
			if got, want := r.Header.Get("Authorization"), "Bearer "+token; got != want {
				http.Error(w, "missing authorization", http.StatusForbidden)
				return
			}
			w.Header().Set("Location", redirectTarget.URL+"/latest")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusFound)
			fmt.Fprint(w, `{"tag_name":"v1.2.3"}`)
		case "/v1.2.3/" + archiveName:
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = w.Write(archive)
		case "/v1.2.3/detent_1.2.3_checksums.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprintf(w, "%s  %s\n", archiveChecksum(archive), archiveName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tmp := t.TempDir()
	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"GITHUB_TOKEN="+token,
		"DETENT_GITHUB_TOKEN=",
		"DETENT_VERSION=",
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
	select {
	case got := <-leaked:
		t.Fatalf("redirect target received Authorization = %q", got)
	default:
	}
}

func TestInstallScriptReportsPathGuidanceWhenInstallDirIsMissingFromPath(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

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
	env := append(os.Environ(),
		"HOME="+tmp,
		"PATH=/usr/bin:/bin",
		"DETENT_INSTALL_SOURCE="+source,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
	)

	result := runInstall(t, root, env)
	if result.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	want := fmt.Sprintf("Add %s to PATH before running detent: export PATH=\"%s:$PATH\"", installDir, installDir)
	if !strings.Contains(result.stdout, want) {
		t.Fatalf("install stdout = %q, want PATH guidance %q", result.stdout, want)
	}
}

func TestInstallScriptReportsActiveServiceRestartGuidance(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

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
	fakeBin := filepath.Join(tmp, "fakebin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(fakebin) error = %v", err)
	}
	fakeSystemctl := filepath.Join(fakeBin, "systemctl")
	if err := os.WriteFile(fakeSystemctl, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(systemctl) error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DETENT_INSTALL_SOURCE="+source,
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
		"DETENT_INSTALL_TEST_UNAME_S=Linux",
		"DETENT_INSTALL_TEST_UNAME_M=x86_64",
	)

	result := runInstall(t, root, env)
	if result.err != nil {
		t.Fatalf("install error = %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}
	for _, want := range []string{
		"running systemd user service was not restarted",
		"systemctl --user restart detent.service",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Fatalf("install stdout = %q, want containing %q", result.stdout, want)
		}
	}
}

func TestInstallScriptAbortsOnChecksumMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

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

func TestInstallScriptBuildsPreparedSourceWhenReleaseAssetMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	archiveName := "detent_1.2.3_source.tar.gz"
	archive := tarArchive(t, map[string]string{
		"detent_1.2.3_source/BUILD_LDFLAGS":                                    "-X main.version=1.2.3 -X main.commit=fixture",
		"detent_1.2.3_source/static/app/conversation/app.js":                   "prepared-client",
		"detent_1.2.3_source/static/app/conversation/app.css":                  "prepared-style",
		"detent_1.2.3_source/static/app/conversation/chunks/lazy.js":           "prepared-lazy",
		"detent_1.2.3_source/static/app/conversation/THIRD_PARTY_LICENSES.txt": "prepared-license",
		"detent_1.2.3_source/static/css/output.css":                            "prepared-dashboard-style",
	})
	tmp := t.TempDir()
	releaseDir := filepath.Join(tmp, "release")
	if err := os.MkdirAll(filepath.Join(releaseDir, "v1.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(releaseDir, "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"releases/latest":                   []byte(`{"tag_name":"v1.2.3"}`),
		"v1.2.3/" + archiveName:             archive,
		"v1.2.3/detent_1.2.3_checksums.txt": []byte(archiveChecksum(archive) + "  " + archiveName + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(releaseDir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	releaseURL := (&url.URL{Scheme: "file", Path: releaseDir}).String()
	fakeBin := filepath.Join(tmp, "fakebin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(fakebin) error = %v", err)
	}
	fakeGo := filepath.Join(fakeBin, "go")
	fakeGoScript := `#!/usr/bin/env sh
set -eu
[ "$1" = build ]
[ "$2" = -ldflags ]
[ "$3" = "-X main.version=1.2.3 -X main.commit=fixture" ]
[ "$4" = -o ]
[ "$(cat static/app/conversation/app.js)" = prepared-client ]
[ "$(cat static/app/conversation/app.css)" = prepared-style ]
[ "$(cat static/app/conversation/chunks/lazy.js)" = prepared-lazy ]
[ "$(cat static/app/conversation/THIRD_PARTY_LICENSES.txt)" = prepared-license ]
[ "$(cat static/css/output.css)" = prepared-dashboard-style ]
cat > "$5" <<'EOF'
#!/usr/bin/env sh
printf 'prepared-source-ok\n'
EOF
chmod 755 "$5"
`
	if err := os.WriteFile(fakeGo, []byte(fakeGoScript), 0o755); err != nil {
		t.Fatalf("WriteFile(fake go) error = %v", err)
	}

	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"HOME="+tmp,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DETENT_GITHUB_API_BASE="+releaseURL,
		"DETENT_RELEASE_DOWNLOAD_BASE="+releaseURL,
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
	if stdout != "prepared-source-ok\n" {
		t.Fatalf("installed fallback binary stdout = %q, want prepared-source-ok", stdout)
	}
}

func TestFreshInstallBootsOnboardingWizardAndExposesRunnerEnrollment(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	tmp := t.TempDir()
	root = isolatedInstallCheckout(t, root, filepath.Join(tmp, "checkout"))
	installDir := filepath.Join(tmp, "bin")
	stateDir := filepath.Join(tmp, "state")
	env := append(os.Environ(),
		"DETENT_INSTALL_DIR="+installDir,
		"DETENT_STATE_DIR="+stateDir,
		"DETENT_INSTALL_LOCK="+filepath.Join(stateDir, "install.lock"),
	)

	install := runInstall(t, root, env)
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
		"CONFIG_HOME="+filepath.Join(home, ".detent"),
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

	assertInstalledEnrollmentCommand(t, binary, tmp, serverEnv)
}

func isolatedInstallCheckout(t *testing.T, source string, checkout string) string {
	t.Helper()

	clone := exec.CommandContext(t.Context(), "git", "clone", "--shared", "--no-checkout", "--", source, checkout)
	if output, err := clone.CombinedOutput(); err != nil {
		t.Fatalf("clone install checkout: %v\n%s", err, output)
	}
	files := exec.CommandContext(t.Context(), "git", "ls-files", "-z")
	files.Dir = source
	output, err := files.Output()
	if err != nil {
		t.Fatalf("list install source: %v", err)
	}
	for _, name := range strings.Split(string(output), "\x00") {
		if name == "" {
			continue
		}
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(checkout, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(link, target); err != nil {
				t.Fatal(err)
			}
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	return checkout
}

func detentArchive(t *testing.T, content string) []byte {
	t.Helper()
	return tarArchive(t, map[string]string{"detent": content})
}

func tarArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveChecksum(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
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

type installerTesting interface {
	Helper()
	Context() context.Context
	Deadline() (time.Time, bool)
}

func runInstall(t installerTesting, root string, env []string) installRun {
	t.Helper()

	ctx, cancel := installerCommandContext(t)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "install.sh")
	cmd.Dir = root
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := runInstallerCommand(ctx, cmd)
	return installRun{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
}

func installerCommandContext(t installerTesting) (context.Context, context.CancelFunc) {
	t.Helper()

	deadline, ok := t.Deadline()
	if !ok {
		return context.WithCancel(t.Context())
	}
	return context.WithDeadline(t.Context(), deadline.Add(-2*installerProcessEvidenceGuard))
}

type installerDeadlineTest struct {
	*testing.T
	deadline time.Time
}

func (t installerDeadlineTest) Deadline() (time.Time, bool) {
	return t.deadline, true
}

func TestInstallCommandDeadlines(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name    string
		cli     bool
		expired bool
		silence time.Duration
	}{
		{name: "installer quiet build", silence: 2*time.Minute + 2*installerProcessEvidenceGuard},
		{name: "installed CLI quiet work", cli: true, silence: testenv.SubprocessWaitTimeout + 2*installerProcessEvidenceGuard},
		{name: "installer expired deadline", expired: true},
		{name: "installed CLI expired deadline", cli: true, expired: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ready := make(chan struct{})
			release := make(chan struct{})
			finish := sync.OnceFunc(func() { close(release) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(ready)
				select {
				case <-release:
					fmt.Fprintln(w, "complete")
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer finish()

			root := t.TempDir()
			script := "#!/bin/sh\nset -eu\ncurl --noproxy '*' -fsS \"$DETENT_INSTALL_TEST_WAIT_URL\"\n"
			if err := os.WriteFile(filepath.Join(root, "install.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(os.Environ(), "DETENT_INSTALL_TEST_WAIT_URL="+server.URL)
			done := make(chan installRun, 1)
			var commandTest installerTesting = t
			if tt.expired {
				commandTest = installerDeadlineTest{T: t}
			}
			go func() {
				if tt.cli {
					stdout, stderr, err := runDetentCommandOutput(commandTest, "sh", root, env, "install.sh")
					done <- installRun{stdout: stdout, stderr: stderr, err: err}
					return
				}
				done <- runInstall(commandTest, root, env)
			}()
			if tt.expired {
				select {
				case result := <-done:
					if !errors.Is(result.err, context.DeadlineExceeded) {
						t.Fatalf("command error = %v, want expired test deadline", result.err)
					}
				case <-ready:
					finish()
					<-done
					t.Fatal("command started after its test deadline")
				case <-time.After(testenv.SubprocessWaitTimeout):
					t.Fatal("command did not stop at its test deadline")
				}
				return
			}
			select {
			case <-ready:
			case result := <-done:
				t.Fatalf("command finished before readiness: %v\nstdout: %q\nstderr: %q", result.err, result.stdout, result.stderr)
			case <-time.After(testenv.SubprocessWaitTimeout):
				t.Fatal("command did not become ready")
			}

			silence := time.NewTimer(tt.silence)
			defer silence.Stop()
			select {
			case result := <-done:
				t.Fatalf("command finished during the quiet work interval: %v\nstdout: %q\nstderr: %q", result.err, result.stdout, result.stderr)
			case <-silence.C:
			}
			finish()
			select {
			case result := <-done:
				if result.err != nil || result.stdout != "complete\n" || result.stderr != "" {
					t.Fatalf("command error = %v, stdout = %q, stderr = %q; want successful silent work and complete output", result.err, result.stdout, result.stderr)
				}
			case <-time.After(testenv.SubprocessWaitTimeout):
				t.Fatal("command did not finish after release")
			}
		})
	}
}

func TestRunInstallerCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Parallel()

	tests := []struct {
		name                string
		script              string
		missing             bool
		cancelWith          error
		expired             bool
		wantStage           string
		wantError           error
		wantExit            int
		wantStdout          string
		wantStderr          string
		wantChild           bool
		exitDuringSnapshot  bool
		snapshotAfterExit   bool
		delaySnapshot       bool
		snapshotUnavailable bool
	}{
		{name: "success", script: "printf stdout; printf stderr >&2", wantStdout: "stdout", wantStderr: "stderr"},
		{name: "expired before start", expired: true, wantStage: "start", wantError: context.DeadlineExceeded},
		{name: "start failure", missing: true, wantStage: "start", wantError: os.ErrNotExist},
		{name: "exit failure", script: "printf stdout; printf stderr >&2; exit 23", wantStage: "wait", wantExit: 23, wantStdout: "stdout", wantStderr: "stderr"},
		{name: "canceled running command", script: "printf ready; read line <&3", cancelWith: context.Canceled, wantStage: "wait", wantError: context.Canceled, wantStdout: "ready", delaySnapshot: true},
		{name: "deadline during wait", script: "printf ready; read line <&3", cancelWith: context.DeadlineExceeded, wantStage: "wait", wantError: context.DeadlineExceeded, wantStdout: "ready", delaySnapshot: true},
		{name: "deadline in nested command", script: "sh -c 'printf ready; read line <&3' & wait", cancelWith: context.DeadlineExceeded, wantStage: "wait", wantError: context.DeadlineExceeded, wantStdout: "ready", wantChild: true, delaySnapshot: true},
		{name: "exit during deadline snapshot", script: "printf ready; read line <&3; exit 0", cancelWith: context.DeadlineExceeded, wantStage: "wait", wantError: context.DeadlineExceeded, wantStdout: "ready", exitDuringSnapshot: true, delaySnapshot: true},
		{name: "deadline during unavailable snapshot", script: "printf ready; read line <&3", cancelWith: context.DeadlineExceeded, wantStage: "wait", wantError: context.DeadlineExceeded, wantStdout: "ready", delaySnapshot: true, snapshotUnavailable: true},
		{name: "exit during unavailable snapshot", script: "printf ready; read line <&3; exit 0", cancelWith: context.DeadlineExceeded, wantStage: "wait", wantError: context.DeadlineExceeded, wantStdout: "ready", exitDuringSnapshot: true, delaySnapshot: true, snapshotUnavailable: true},
		{name: "completed before snapshot", script: "printf ready; read line <&3; exit 0", cancelWith: context.DeadlineExceeded, wantStdout: "ready", exitDuringSnapshot: true, snapshotAfterExit: true},
		{name: "descendant holds stdout", script: "(read line <&3) 2>/dev/null & printf stdout; exit 0", wantStage: "output drain", wantError: exec.ErrWaitDelay, wantStdout: "stdout"},
		{name: "descendant holds stderr", script: "(read line <&3) >/dev/null & printf stderr >&2; exit 0", wantStage: "output drain", wantError: exec.ErrWaitDelay, wantStderr: "stderr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			if tt.expired {
				var expire context.CancelFunc
				ctx, expire = context.WithDeadline(ctx, time.Time{})
				defer expire()
			}
			input, release, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer release.Close()
			exited, inherited, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer exited.Close()
			defer inherited.Close()

			cmd := exec.CommandContext(ctx, "sh", "-c", tt.script)
			if tt.missing {
				cmd = exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-command"))
			}
			cmd.ExtraFiles = []*os.File{input, inherited}
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if tt.cancelWith != nil {
				cmd.Stdout = installerReadyWriter{output: &stdout, cancel: func() { cancel(tt.cancelWith) }}
			}

			snapshotStarted := make(chan struct{}, 1)
			snapshotRelease := make(chan struct{})
			exitObserved := make(chan error, 1)
			snapshot := func(group int) installerProcessSnapshot {
				if tt.delaySnapshot {
					snapshotStarted <- struct{}{}
					<-snapshotRelease
				}
				var evidence installerProcessSnapshot
				if tt.snapshotUnavailable {
					evidence = installerProcessSnapshot{description: "snapshot unavailable: signal: killed; context deadline exceeded"}
				} else if !tt.snapshotAfterExit {
					evidence = installerProcessEvidence(group)
				}
				if !tt.exitDuringSnapshot {
					return evidence
				}
				_ = inherited.Close()
				_ = release.Close()
				// Exit ordering is descriptor-driven; this deadline only guards against a stuck subprocess.
				if deadlineErr := exited.SetReadDeadline(time.Now().Add(10 * time.Second)); deadlineErr != nil {
					exitObserved <- deadlineErr
					return evidence
				}
				_, readErr := exited.Read(make([]byte, 1))
				exitObserved <- readErr
				if tt.snapshotAfterExit {
					return installerProcessEvidence(group)
				}
				return evidence
			}
			done := make(chan error, 1)
			go func() { done <- runInstallerCommandWithEvidence(ctx, cmd, snapshot) }()
			if tt.delaySnapshot {
				select {
				case <-snapshotStarted:
					close(snapshotRelease)
				case err = <-done:
					t.Fatalf("installer command finished before process snapshot started: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("installer command did not start process snapshot")
				}
			}
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				cancel(nil)
				_ = release.Close()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("installer command did not stop after cancellation and pipe release")
				}
				t.Fatal("installer command did not finish; output pipe remained open")
			}
			_ = inherited.Close()
			if tt.exitDuringSnapshot {
				if readErr := <-exitObserved; !errors.Is(readErr, io.EOF) {
					t.Fatalf("command did not exit while collecting process evidence: %v", readErr)
				}
			}
			if deadlineErr := exited.SetReadDeadline(time.Now().Add(10 * time.Second)); deadlineErr != nil {
				t.Fatal(deadlineErr)
			}
			if _, readErr := exited.Read(make([]byte, 1)); !errors.Is(readErr, io.EOF) {
				t.Fatalf("command descendants still hold inherited descriptor: %v", readErr)
			}
			if tt.wantStage == "" {
				if err != nil {
					t.Fatalf("runInstallerCommand() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantStage) || !strings.Contains(err.Error(), cmd.Path) {
				t.Fatalf("runInstallerCommand() error = %v, want command path and stage %q", err, tt.wantStage)
			}
			if tt.wantError != nil && !errors.Is(err, tt.wantError) {
				t.Fatalf("runInstallerCommand() error = %v, want %v", err, tt.wantError)
			}
			if tt.cancelWith != nil && tt.wantError != nil {
				want := fmt.Sprintf("pid=%d ppid=", cmd.Process.Pid)
				if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "executable=") {
					t.Fatalf("runInstallerCommand() error = %v, want process evidence containing %q and executable", err, want)
				}
			}
			if tt.snapshotUnavailable && !strings.Contains(err.Error(), "snapshot unavailable: signal: killed; context deadline exceeded") {
				t.Fatalf("runInstallerCommand() error = %v, want unavailable snapshot cause", err)
			}
			if tt.wantChild && !strings.Contains(err.Error(), fmt.Sprintf("ppid=%d pgid=%d", cmd.Process.Pid, cmd.Process.Pid)) {
				t.Fatalf("runInstallerCommand() error = %v, want nested command captured before cleanup", err)
			}
			if tt.wantExit != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.wantExit {
					t.Fatalf("runInstallerCommand() error = %v, want exit code %d", err, tt.wantExit)
				}
			}
			if stdout.String() != tt.wantStdout || stderr.String() != tt.wantStderr {
				t.Fatalf("output = (%q, %q), want (%q, %q)", stdout.String(), stderr.String(), tt.wantStdout, tt.wantStderr)
			}
		})
	}
}

type installerReadyWriter struct {
	output io.Writer
	cancel context.CancelFunc
}

func (w installerReadyWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	w.cancel()
	return n, err
}

func runInstallerCommand(ctx context.Context, cmd *exec.Cmd) error {
	return runInstallerCommandWithEvidence(ctx, cmd, installerProcessEvidence)
}

func runInstallerCommandWithEvidence(ctx context.Context, cmd *exec.Cmd, snapshot func(int) installerProcessSnapshot) error {
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var processEvidence installerProcessSnapshot
	cmd.Cancel = func() error {
		processEvidence = installerProcessSnapshotWithIdentity(cmd, snapshot(cmd.Process.Pid))
		return terminateInstallerProcessGroup(cmd.Process.Pid, processEvidence, syscall.Kill)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("command %q args=%q stage=start elapsed=%s: %w", cmd.Path, cmd.Args[1:], time.Since(started).Round(time.Millisecond), errors.Join(err, context.Cause(ctx)))
	}
	startDuration := time.Since(started)
	waiting := time.Now()
	err := cmd.Wait()
	if err == nil && processEvidence.live {
		err = context.Cause(ctx)
	}
	if err == nil {
		return nil
	}
	waitDuration := time.Since(waiting)
	if processEvidence.description == "" {
		processEvidence = installerProcessSnapshotWithIdentity(cmd, snapshot(cmd.Process.Pid))
	}
	if killErr := terminateInstallerProcessGroup(cmd.Process.Pid, processEvidence, syscall.Kill); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		err = errors.Join(err, fmt.Errorf("clean up command process group: %w", killErr))
	}
	stage := "wait"
	if errors.Is(err, exec.ErrWaitDelay) {
		stage = "output drain"
	}
	return fmt.Errorf("command %q args=%q pid=%d stage=%s start=%s wait=%s processes=%q: %w", cmd.Path, cmd.Args[1:], cmd.Process.Pid, stage,
		startDuration.Round(time.Millisecond), waitDuration.Round(time.Millisecond), processEvidence.description, errors.Join(err, context.Cause(ctx)))
}

func installerProcessSnapshotWithIdentity(cmd *exec.Cmd, snapshot installerProcessSnapshot) installerProcessSnapshot {
	if snapshot.available {
		return snapshot
	}
	identity := fmt.Sprintf("pid=%d ppid=%d pgid=%d state=unknown executable=%s", cmd.Process.Pid, os.Getpid(), cmd.Process.Pid, cmd.Path)
	if snapshot.description != "" {
		identity += "; " + snapshot.description
	}
	snapshot.description = identity
	// Cancellation began while exec still owned this process. Retain that fact
	// when the diagnostic subprocess cannot determine the current group state.
	snapshot.live = true
	return snapshot
}

type installerProcessSnapshot struct {
	description string
	available   bool
	live        bool
}

func terminateInstallerProcessGroup(group int, snapshot installerProcessSnapshot, kill func(int, syscall.Signal) error) error {
	if snapshot.available && !snapshot.live {
		return os.ErrProcessDone
	}
	err := kill(-group, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

const installerProcessEvidenceGuard = 10 * time.Second

func installerProcessEvidence(group int) installerProcessSnapshot {
	return installerProcessEvidenceWithRunner(group, context.WithTimeout, func(ctx context.Context) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,pgid=,stat=,comm=")
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		cmd.WaitDelay = time.Second
		return cmd.Output()
	})
}

func installerProcessEvidenceWithRunner(
	group int,
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc),
	run func(context.Context) ([]byte, error),
) installerProcessSnapshot {
	// This deadline is an OS deadlock guard, not part of installer timeout behavior.
	ctx, cancel := withTimeout(context.Background(), installerProcessEvidenceGuard)
	defer cancel()
	output, err := run(ctx)
	if err != nil {
		return installerProcessSnapshot{description: fmt.Sprintf("snapshot unavailable: %v", errors.Join(err, context.Cause(ctx)))}
	}
	return installerProcessGroup(string(output), group)
}

func TestInstallerProcessEvidenceWaitsForDelayedSnapshot(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	requestedGuard := make(chan time.Duration, 1)
	done := make(chan installerProcessSnapshot, 1)
	go func() {
		done <- installerProcessEvidenceWithRunner(100, func(parent context.Context, guard time.Duration) (context.Context, context.CancelFunc) {
			requestedGuard <- guard
			return context.WithCancel(parent)
		}, func(context.Context) ([]byte, error) {
			close(started)
			<-release
			return []byte("100 1 100 S /bin/sh\n"), nil
		})
	}()
	select {
	case guard := <-requestedGuard:
		if guard != 10*time.Second {
			t.Fatalf("installer process evidence guard = %s, want 10s", guard)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installerProcessEvidenceWithRunner() did not request a timeout")
	}
	select {
	case <-started:
	case snapshot := <-done:
		t.Fatalf("installerProcessEvidenceWithRunner() returned before starting the snapshot: %+v", snapshot)
	case <-time.After(10 * time.Second):
		t.Fatal("installerProcessEvidenceWithRunner() did not start the snapshot")
	}
	select {
	case snapshot := <-done:
		t.Fatalf("installerProcessEvidenceWithRunner() returned before delayed snapshot was released: %+v", snapshot)
	default:
	}
	close(release)
	// Snapshot release is channel-driven; this timeout only guards against a leaked goroutine.
	select {
	case snapshot := <-done:
		if snapshot.description != "pid=100 ppid=1 pgid=100 state=S executable=/bin/sh" || !snapshot.available || !snapshot.live {
			t.Fatalf("installerProcessEvidenceWithRunner() = %+v, want delayed process evidence", snapshot)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installerProcessEvidenceWithRunner() did not return after delayed snapshot was released")
	}
}

func installerProcessGroup(output string, group int) installerProcessSnapshot {
	var processes []string
	snapshot := installerProcessSnapshot{available: true}
	for line := range strings.SplitSeq(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[2] != strconv.Itoa(group) {
			continue
		}
		processes = append(processes, fmt.Sprintf("pid=%s ppid=%s pgid=%s state=%s executable=%s", fields[0], fields[1], fields[2], fields[3], strings.Join(fields[4:], " ")))
		if !strings.HasPrefix(fields[3], "Z") {
			snapshot.live = true
		}
	}
	if len(processes) == 0 {
		snapshot.description = "no process group members observed"
		return snapshot
	}
	snapshot.description = strings.Join(processes, "; ")
	return snapshot
}

func TestInstallerProcessGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   string
		want     string
		wantLive bool
	}{
		{name: "empty", want: "no process group members observed"},
		{name: "unrelated groups", output: "123 1 123 S unrelated\n456 1 456 R other", want: "no process group members observed"},
		{name: "malformed rows", output: "\n100 1 100\n100 1 invalid S sh", want: "no process group members observed"},
		{name: "shell and child", output: " 100 1 100 S /bin/sh\n101 100 100 R /bin/rm\n102 1 102 S unrelated", want: "pid=100 ppid=1 pgid=100 state=S executable=/bin/sh; pid=101 ppid=100 pgid=100 state=R executable=/bin/rm", wantLive: true},
		{name: "executable contains spaces", output: "101 100 100 S /path with spaces/tool", want: "pid=101 ppid=100 pgid=100 state=S executable=/path with spaces/tool", wantLive: true},
		{name: "child after shell exit", output: "101 1 100 S rm", want: "pid=101 ppid=1 pgid=100 state=S executable=rm", wantLive: true},
		{name: "zombie only", output: "100 1 100 Z sh", want: "pid=100 ppid=1 pgid=100 state=Z executable=sh"},
		{name: "zombie parent with live child", output: "100 1 100 Z sh\n101 100 100 S rm", want: "pid=100 ppid=1 pgid=100 state=Z executable=sh; pid=101 ppid=100 pgid=100 state=S executable=rm", wantLive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := installerProcessGroup(tt.output, 100)
			if got.description != tt.want || !got.available || got.live != tt.wantLive {
				t.Fatalf("installerProcessGroup() = %+v, want description %q, available=true, and live=%t", got, tt.want, tt.wantLive)
			}
		})
	}
}

func TestTerminateInstallerProcessGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		snapshot   installerProcessSnapshot
		killErr    error
		wantErr    error
		wantCalled bool
	}{
		{name: "unavailable snapshot kills", snapshot: installerProcessSnapshot{description: "snapshot unavailable"}, wantCalled: true},
		{name: "live group kills", snapshot: installerProcessSnapshot{available: true, live: true}, wantCalled: true},
		{name: "empty group is already done", snapshot: installerProcessSnapshot{available: true}, wantErr: os.ErrProcessDone},
		{name: "zombie-only group is already done", snapshot: installerProcessSnapshot{available: true, description: "pid=100 state=Z"}, wantErr: os.ErrProcessDone},
		{name: "missing group is already done", killErr: syscall.ESRCH, wantErr: os.ErrProcessDone, wantCalled: true},
		{name: "kill failure is preserved", killErr: syscall.EPERM, wantErr: syscall.EPERM, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			got := terminateInstallerProcessGroup(100, tt.snapshot, func(pid int, signal syscall.Signal) error {
				called = true
				if pid != -100 || signal != syscall.SIGKILL {
					t.Fatalf("kill() = (%d, %d), want (-100, %d)", pid, signal, syscall.SIGKILL)
				}
				return tt.killErr
			})
			if !errors.Is(got, tt.wantErr) || called != tt.wantCalled {
				t.Fatalf("terminateInstallerProcessGroup() = %v, called=%t; want error %v, called=%t", got, called, tt.wantErr, tt.wantCalled)
			}
		})
	}
}

func reservePort(t *testing.T) int {
	t.Helper()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
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

	cmd := exec.CommandContext(t.Context(), binary, "--host", "127.0.0.1", "--port", strconv.Itoa(port))
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

func assertInstalledEnrollmentCommand(t *testing.T, binary string, root string, env []string) {
	t.Helper()

	stdout, stderr, err := runDetentCommandOutput(t, binary, root, env, "hub", "runner", "register", "--help")
	if err != nil {
		t.Fatalf("installed runner enrollment help: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{"--url", "--token", "--config", "--workspace-root", "--service"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("installed runner enrollment help missing %q:\n%s", want, stdout)
		}
	}
}

func runDetentCommandOutput(t installerTesting, binary string, workdir string, env []string, args ...string) (string, string, error) {
	t.Helper()

	ctx, cancel := installerCommandContext(t)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workdir
	cmd.Env = env

	var stdout strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := runInstallerCommand(ctx, cmd)
	return stdout.String(), stderr.String(), err
}

func assertInstalledVersionMetadata(t *testing.T, binary string, root string, env []string) {
	t.Helper()

	wantVersion, ok := gitOutput(t, root, "describe", "--tags", "--always")
	if !ok {
		t.Skip("git metadata unavailable")
	}
	wantCommit, ok := gitOutput(t, root, "rev-parse", "HEAD")
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
	var got struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildDate string `json:"build_date"`
		GoVersion string `json:"go_version"`
		OS        string `json:"os"`
		Arch      string `json:"arch"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("detent version JSON error = %v\nstdout:\n%s", err, stdout)
	}
	if got.Version != wantVersion || got.Commit != wantCommit {
		t.Fatalf("detent version metadata = %#v, want version %q commit %q", got, wantVersion, wantCommit)
	}
	if got.GoVersion != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Fatalf("detent version runtime metadata = %#v", got)
	}
	if got.BuildDate == "unknown" {
		t.Fatalf("detent version output kept unknown build date:\n%s", stdout)
	}
}

func gitOutput(t *testing.T, root string, args ...string) (string, bool) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
