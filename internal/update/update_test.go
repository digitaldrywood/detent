package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v1.2.4\n", nil
		},
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

func TestServiceGoInstallYesRunsCommandAndReportsVersion(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	goBin := filepath.Join(tmp, "gobin")
	binary := filepath.Join(goBin, "detent")
	if err := os.MkdirAll(goBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(goBin) error = %v", err)
	}
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var gotCommand string
	var gotArgs []string
	service := NewService(Config{
		CurrentVersion: "1.2.3",
		ExecutablePath: binary,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: staticReleaseClient{
			releases: []Release{{TagName: "v1.2.4"}},
		},
		Env: map[string]string{"GOBIN": goBin},
		CommandRunner: func(_ context.Context, command string, args []string, out io.Writer, errOut io.Writer) error {
			gotCommand = command
			gotArgs = append(gotArgs, args...)
			_, _ = fmt.Fprintln(out, "go install output")
			return nil
		},
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v1.2.4\ncommit: abc1234\n", nil
		},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{
		AssumeYes: true,
		Stdout:    &stdout,
		Stderr:    &stderr,
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if status.Action != ActionUpdated {
		t.Fatalf("Action = %q, want %q", status.Action, ActionUpdated)
	}
	if gotCommand != "go" {
		t.Fatalf("command = %q, want go", gotCommand)
	}
	if strings.Join(gotArgs, " ") != "install github.com/digitaldrywood/detent/cmd/detent@latest" {
		t.Fatalf("args = %q, want go install module", gotArgs)
	}
	if !strings.Contains(stdout.String(), "go install output") {
		t.Fatalf("stdout = %q, want streamed go install output", stdout.String())
	}
	if !strings.Contains(status.Message, "Installed Detent version: v1.2.4") {
		t.Fatalf("Message = %q, want installed version", status.Message)
	}
	if !strings.Contains(status.Message, "Restart Detent") {
		t.Fatalf("Message = %q, want restart note", status.Message)
	}
	if status.Command != moduleInstallCommand {
		t.Fatalf("Command = %q, want %q", status.Command, moduleInstallCommand)
	}
}

func TestServiceGoInstallInteractiveAbortReturnsCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	goBin := filepath.Join(tmp, "gobin")
	binary := filepath.Join(goBin, "detent")
	if err := os.MkdirAll(goBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(goBin) error = %v", err)
	}
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}

	service := NewService(Config{
		CurrentVersion: "1.2.3",
		ExecutablePath: binary,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: staticReleaseClient{
			releases: []Release{{TagName: "v1.2.4"}},
		},
		Env: map[string]string{"GOBIN": goBin},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{
		SelectGoInstallAction: func(Status) (GoInstallAction, error) {
			return GoInstallActionAbort, nil
		},
	})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Apply() error = %v, want %v", err, ErrRefused)
	}
	if status.Action != ActionRefused {
		t.Fatalf("Action = %q, want %q", status.Action, ActionRefused)
	}
	if status.Command != moduleInstallCommand {
		t.Fatalf("Command = %q, want %q", status.Command, moduleInstallCommand)
	}
	if !strings.Contains(status.Message, "Update aborted") {
		t.Fatalf("Message = %q, want abort message", status.Message)
	}
}

func TestServiceGoInstallFromReleaseUsesReleaseAsset(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	goBin := filepath.Join(tmp, "gobin")
	binary := filepath.Join(goBin, "detent")
	if err := os.MkdirAll(goBin, 0o755); err != nil {
		t.Fatalf("MkdirAll(goBin) error = %v", err)
	}
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(binary) error = %v", err)
	}

	archiveName := "detent_1.2.4_linux_amd64.tar.gz"
	checksumName := "detent_1.2.4_checksums.txt"
	archive := detentUpdateArchive(t, "updated")
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%x  %s\n", sum, archiveName)
	downloads := map[string][]byte{
		"https://example.invalid/archive":   archive,
		"https://example.invalid/checksums": []byte(checksums),
	}
	var stderr bytes.Buffer
	var verifiedPath string
	service := NewService(Config{
		CurrentVersion: "1.2.3",
		ExecutablePath: binary,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: staticReleaseClient{
			releases: []Release{{
				TagName: "v1.2.4",
				Assets: []Asset{
					{Name: archiveName, BrowserDownloadURL: "https://example.invalid/archive"},
					{Name: checksumName, BrowserDownloadURL: "https://example.invalid/checksums"},
				},
			}},
			downloads: downloads,
		},
		Env: map[string]string{"GOBIN": goBin},
		BinaryVerifier: func(_ context.Context, path string) (string, error) {
			verifiedPath = path
			return "version: v1.2.4\n", nil
		},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{
		FromRelease: true,
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if status.Action != ActionUpdated {
		t.Fatalf("Action = %q, want %q", status.Action, ActionUpdated)
	}
	if status.Asset != archiveName {
		t.Fatalf("Asset = %q, want %q", status.Asset, archiveName)
	}
	if verifiedPath != binary {
		t.Fatalf("verifiedPath = %q, want %q", verifiedPath, binary)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if strings.TrimSpace(string(raw)) != "updated" {
		t.Fatalf("updated binary = %q, want updated", raw)
	}
	if !strings.Contains(stderr.String(), "WARNING:") {
		t.Fatalf("stderr = %q, want release-swap warning", stderr.String())
	}
	if !strings.Contains(status.Message, "Restart Detent") {
		t.Fatalf("Message = %q, want restart note", status.Message)
	}
}

func TestReplaceBinaryPreservesPermissionsAndVerifies(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o750); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	var verifiedPath string
	err := ReplaceBinary(Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o600,
		GOOS:   "linux",
		Verify: func(_ context.Context, path string) (string, error) {
			verifiedPath = path
			return "version: v1.2.4\n", nil
		},
	})
	if err != nil {
		t.Fatalf("ReplaceBinary() error = %v", err)
	}
	if verifiedPath != target {
		t.Fatalf("verifiedPath = %q, want %q", verifiedPath, target)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(raw) != "new" {
		t.Fatalf("target = %q, want new", raw)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat(target) error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Fatalf("mode = %v, want 0750", got)
	}
}

func TestReplaceBinaryRollsBackWhenVerificationFails(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	err := ReplaceBinary(Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "linux",
		Verify: func(context.Context, string) (string, error) {
			return "", errors.New("verification failed")
		},
	})
	if err == nil {
		t.Fatal("ReplaceBinary() error = nil, want verification failure")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(raw) != "old" {
		t.Fatalf("target = %q, want rollback to old", raw)
	}
}

func TestReplaceBinarySignsDarwinBeforeVerify(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	var calls []string
	err := ReplaceBinary(Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "darwin",
		Sign: func(_ context.Context, path string) error {
			calls = append(calls, "sign:"+path)
			return nil
		},
		Verify: func(_ context.Context, path string) (string, error) {
			calls = append(calls, "verify:"+path)
			return "version: v1.2.4\n", nil
		},
	})
	if err != nil {
		t.Fatalf("ReplaceBinary() error = %v", err)
	}
	want := []string{"sign:" + target, "verify:" + target}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestReplaceBinaryStagesWindowsReplacement(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	var startedCommand string
	var startedArgs []string
	err := ReplaceBinary(Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "windows",
		StartProcess: func(command string, args []string) error {
			startedCommand = command
			startedArgs = append(startedArgs, args...)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ReplaceBinary() error = %v", err)
	}

	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(raw) != "old" {
		t.Fatalf("target = %q, want original binary to remain until handoff runs", raw)
	}
	if startedCommand != "cmd.exe" {
		t.Fatalf("startedCommand = %q, want cmd.exe", startedCommand)
	}
	if len(startedArgs) == 0 {
		t.Fatal("startedArgs is empty")
	}

	stagedBinary, script := stagedWindowsUpdateFiles(t, tmp)
	stagedRaw, err := os.ReadFile(stagedBinary)
	if err != nil {
		t.Fatalf("ReadFile(staged binary) error = %v", err)
	}
	if string(stagedRaw) != "new" {
		t.Fatalf("staged binary = %q, want new", stagedRaw)
	}

	scriptRaw, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("ReadFile(script) error = %v", err)
	}
	scriptText := string(scriptRaw)
	if !strings.Contains(scriptText, `move /Y "%source%" "%target%"`) {
		t.Fatalf("script does not move staged binary into place:\n%s", scriptText)
	}
	if !strings.Contains(scriptText, `timeout /t 1 /nobreak`) {
		t.Fatalf("script does not wait for the running binary lock:\n%s", scriptText)
	}

	joinedArgs := strings.Join(startedArgs, "\n")
	if !strings.Contains(joinedArgs, script) {
		t.Fatalf("startedArgs = %q, want script path %q", startedArgs, script)
	}
}

func TestExtractBinaryReadsWindowsArchive(t *testing.T) {
	t.Parallel()

	archive := detentWindowsUpdateArchive(t, "updated")
	raw, _, err := ExtractBinary(archive, "windows")
	if err != nil {
		t.Fatalf("ExtractBinary() error = %v", err)
	}
	if string(raw) != "updated" {
		t.Fatalf("binary = %q, want updated", raw)
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

func detentWindowsUpdateArchive(t *testing.T, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writer, err := zw.Create("detent.exe")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	return buf.Bytes()
}

type staticReleaseClient struct {
	releases  []Release
	downloads map[string][]byte
}

func (c staticReleaseClient) ListReleases(context.Context) ([]Release, error) {
	return c.releases, nil
}

func (c staticReleaseClient) Download(_ context.Context, url string) ([]byte, error) {
	raw, ok := c.downloads[url]
	if !ok {
		return nil, fmt.Errorf("download not found: %s", url)
	}
	return raw, nil
}

func stagedWindowsUpdateFiles(t *testing.T, dir string) (string, string) {
	t.Helper()

	var stagedBinary string
	var script string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, ".detent.exe.update-"):
			stagedBinary = filepath.Join(dir, name)
		case strings.HasPrefix(name, ".detent-update-") && strings.HasSuffix(name, ".cmd"):
			script = filepath.Join(dir, name)
		}
	}
	if stagedBinary == "" {
		t.Fatal("staged binary was not created")
	}
	if script == "" {
		t.Fatal("update script was not created")
	}
	return stagedBinary, script
}
