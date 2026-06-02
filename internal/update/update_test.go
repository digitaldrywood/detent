package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{name: "equal with v prefix and build metadata", a: "v1.2.3", b: "1.2.3+build.7", want: 0},
		{name: "patch greater", a: "1.2.4", b: "1.2.3", want: 1},
		{name: "minor greater", a: "1.10.0", b: "1.9.9", want: 1},
		{name: "release greater than prerelease", a: "1.2.3", b: "1.2.3-rc.1", want: 1},
		{name: "prerelease identifiers compare numerically", a: "1.2.3-rc.2", b: "1.2.3-rc.1", want: 1},
		{name: "lower major", a: "1.9.9", b: "2.0.0", want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := CompareVersions(tt.a, tt.b)
			if err != nil {
				t.Fatalf("CompareVersions() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestSelectLatestReleasePrereleaseHandling(t *testing.T) {
	t.Parallel()

	releases := []Release{
		{TagName: "v1.3.0-rc.2", Prerelease: true},
		{TagName: "v1.2.4", Prerelease: false},
		{TagName: "v2.0.0", Draft: true},
		{TagName: "not-semver"},
	}

	stable, ok, err := SelectLatestRelease("1.2.3", releases)
	if err != nil {
		t.Fatalf("SelectLatestRelease() stable error = %v", err)
	}
	if !ok {
		t.Fatal("SelectLatestRelease() stable ok = false, want true")
	}
	if stable.TagName != "v1.2.4" {
		t.Fatalf("stable TagName = %q, want v1.2.4", stable.TagName)
	}

	prerelease, ok, err := SelectLatestRelease("1.3.0-rc.1", releases)
	if err != nil {
		t.Fatalf("SelectLatestRelease() prerelease error = %v", err)
	}
	if !ok {
		t.Fatal("SelectLatestRelease() prerelease ok = false, want true")
	}
	if prerelease.TagName != "v1.3.0-rc.2" {
		t.Fatalf("prerelease TagName = %q, want v1.3.0-rc.2", prerelease.TagName)
	}
}

func TestDetectInstallSource(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	releaseBin := filepath.Join(tmp, "bin", "detent")
	goBin := filepath.Join(tmp, "gobin")
	goInstalled := filepath.Join(goBin, "detent")
	brewLink := filepath.Join(tmp, "homebrew", "bin", "detent")
	brewTarget := filepath.Join(tmp, "homebrew", "Cellar", "detent", "1.2.3", "bin", "detent")
	lockPath := filepath.Join(tmp, "state", "install.lock")

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("binary="+releaseBin+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	evalSymlinks := func(path string) (string, error) {
		if path == brewLink {
			return brewTarget, nil
		}
		return path, nil
	}

	tests := []struct {
		name           string
		currentVersion string
		executable     string
		env            map[string]string
		want           InstallSource
		wantCommand    string
	}{
		{
			name:           "release installer lock",
			currentVersion: "1.2.3",
			executable:     releaseBin,
			env:            map[string]string{"DETENT_INSTALL_LOCK": lockPath},
			want:           InstallSourceRelease,
		},
		{
			name:           "homebrew cellar target",
			currentVersion: "1.2.3",
			executable:     brewLink,
			want:           InstallSourceHomebrew,
			wantCommand:    "brew upgrade digitaldrywood/tap/detent",
		},
		{
			name:           "go install bin",
			currentVersion: "dev",
			executable:     goInstalled,
			env:            map[string]string{"GOBIN": goBin},
			want:           InstallSourceGoInstall,
			wantCommand:    "go install github.com/digitaldrywood/detent/cmd/detent@latest",
		},
		{
			name:           "development build",
			currentVersion: "dev",
			executable:     filepath.Join(tmp, "checkout", "tmp", "detent"),
			want:           InstallSourceDevelopment,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DetectInstallSource(DetectionOptions{
				CurrentVersion: tt.currentVersion,
				ExecutablePath: tt.executable,
				GOOS:           "linux",
				HomeDir:        home,
				Env:            tt.env,
				EvalSymlinks:   evalSymlinks,
			})
			if got.Source != tt.want {
				t.Fatalf("Source = %q, want %q", got.Source, tt.want)
			}
			if tt.wantCommand != "" && got.Command != tt.wantCommand {
				t.Fatalf("Command = %q, want %q", got.Command, tt.wantCommand)
			}
		})
	}
}

func TestSelectReleaseAssetsAndVerifyChecksum(t *testing.T) {
	t.Parallel()

	archiveName := "detent_1.2.4_linux_amd64.tar.gz"
	archive := []byte("archive")
	sum := sha256.Sum256(archive)
	checksums := []byte(fmt.Sprintf("%x  %s\n", sum, archiveName))

	assets, err := SelectReleaseAssets(Release{
		TagName: "v1.2.4",
		Assets: []Asset{
			{Name: archiveName, BrowserDownloadURL: "https://example.invalid/archive"},
			{Name: "detent_1.2.4_checksums.txt", BrowserDownloadURL: "https://example.invalid/checksums"},
		},
	}, "linux", "amd64")
	if err != nil {
		t.Fatalf("SelectReleaseAssets() error = %v", err)
	}
	if assets.Archive.Name != archiveName {
		t.Fatalf("Archive.Name = %q, want %q", assets.Archive.Name, archiveName)
	}
	if err := VerifyChecksum(checksums, archiveName, archive); err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if err := VerifyChecksum(checksums, archiveName, []byte("different")); err == nil {
		t.Fatal("VerifyChecksum() error = nil, want checksum mismatch")
	}
}

func TestServiceAppliesReleaseUpdateFromHTTPServer(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	binary := filepath.Join(tmp, "bin", "detent")
	lockPath := filepath.Join(tmp, "state", "install.lock")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatalf("MkdirAll(binary dir) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(lock dir) error = %v", err)
	}
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("binary="+binary+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock) error = %v", err)
	}

	archiveName := "detent_1.2.4_linux_amd64.tar.gz"
	checksumName := "detent_1.2.4_checksums.txt"
	archive := detentUpdateArchive(t, "updated")
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%x  %s\n", sum, archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"tag_name":"v1.2.4","draft":false,"prerelease":false,"assets":[{"name":"%s","browser_download_url":"%s/archive"},{"name":"%s","browser_download_url":"%s/checksums"}]}]`, archiveName, server.URL, checksumName, server.URL)
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	service := NewService(Config{
		CurrentVersion: "1.2.3",
		ExecutablePath: binary,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: NewGitHubClient(GitHubClientConfig{
			APIBase:    server.URL,
			HTTPClient: server.Client(),
		}),
		Env: map[string]string{"DETENT_INSTALL_LOCK": lockPath},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{AssumeYes: true})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if status.Action != ActionUpdated {
		t.Fatalf("Action = %q, want %q", status.Action, ActionUpdated)
	}
	if !status.UpdateAvailable {
		t.Fatal("UpdateAvailable = false, want true")
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if strings.TrimSpace(string(raw)) != "updated" {
		t.Fatalf("updated binary = %q, want updated", raw)
	}
}

func detentUpdateArchive(t *testing.T, content string) []byte {
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
