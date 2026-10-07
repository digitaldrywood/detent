package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"

	"golang.org/x/crypto/blake2b"
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

func TestParseBinaryIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		output      string
		wantVersion string
		wantCommit  string
		wantErr     string
	}{
		{
			name:        "pretty output",
			output:      "version: v1.2.4\ncommit: " + testUpdatedCommit + "\n",
			wantVersion: "v1.2.4",
			wantCommit:  testUpdatedCommit,
		},
		{
			name:        "non-TTY JSON output",
			output:      `{"version":"v1.2.4","commit":"` + testUpdatedCommit + `","build_date":"2026-09-11T23:00:00Z"}`,
			wantVersion: "v1.2.4",
			wantCommit:  testUpdatedCommit,
		},
		{
			name:    "missing commit",
			output:  `{"version":"v1.2.4"}`,
			wantErr: "did not report a full commit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, commit, err := parseBinaryIdentity(tt.output)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseBinaryIdentity() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBinaryIdentity() error = %v", err)
			}
			if version != tt.wantVersion || commit != tt.wantCommit {
				t.Fatalf("parseBinaryIdentity() = %q, %q, want %q, %q", version, commit, tt.wantVersion, tt.wantCommit)
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
			wantCommand:    "detent update --from-release",
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
	signatureName := "detent_1.2.4_checksums.txt.minisig"
	archive := []byte("archive")
	sum := sha256.Sum256(archive)
	checksums := fmt.Appendf(nil, "%x  %s\n", sum, archiveName)

	assets, err := SelectReleaseAssets(Release{
		TagName: "v1.2.4",
		Assets: []Asset{
			{Name: archiveName, BrowserDownloadURL: "https://example.invalid/archive"},
			{Name: "detent_1.2.4_checksums.txt", BrowserDownloadURL: "https://example.invalid/checksums"},
			{Name: signatureName, BrowserDownloadURL: "https://example.invalid/checksums.minisig"},
			{Name: provenanceAssetName, BrowserDownloadURL: "https://example.invalid/provenance"},
		},
	}, "linux", "amd64")
	if err != nil {
		t.Fatalf("SelectReleaseAssets() error = %v", err)
	}
	if assets.Archive.Name != archiveName {
		t.Fatalf("Archive.Name = %q, want %q", assets.Archive.Name, archiveName)
	}
	if assets.ChecksumSignature.Name != signatureName {
		t.Fatalf("ChecksumSignature.Name = %q, want %q", assets.ChecksumSignature.Name, signatureName)
	}
	if err := VerifyChecksum(checksums, archiveName, archive); err != nil {
		t.Fatalf("VerifyChecksum() error = %v", err)
	}
	if err := VerifyChecksum(checksums, archiveName, []byte("different")); err == nil {
		t.Fatal("VerifyChecksum() error = nil, want checksum mismatch")
	}
}

func TestVerifyMinisignSignatureRejectsTamper(t *testing.T) {
	t.Parallel()

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	keyID := []byte("12345678")
	trustedComment := "detent checksums v1.2.4"
	checksums := []byte("abc123  detent_1.2.4_linux_amd64.tar.gz\n")
	minisignPublicKey := testMinisignPublicKey(publicKey, keyID)
	signature := testMinisignSignature(t, privateKey, keyID, checksums, trustedComment)

	tests := []struct {
		name      string
		checksums []byte
		signature []byte
		wantErr   string
	}{
		{
			name:      "valid",
			checksums: checksums,
			signature: signature,
		},
		{
			name:      "tampered checksums",
			checksums: []byte("abc123  detent_1.2.4_darwin_amd64.tar.gz\n"),
			signature: signature,
			wantErr:   "invalid minisign signature",
		},
		{
			name:      "tampered signature",
			checksums: checksums,
			signature: append([]byte("x"), signature[1:]...),
			wantErr:   "parse minisign signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := VerifyMinisignSignature(minisignPublicKey, tt.checksums, tt.signature)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyMinisignSignature() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("VerifyMinisignSignature() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyMinisignSignature() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyChecksumSignatureFailsClosedWithoutPinnedKey(t *testing.T) {
	previous := defaultChecksumMinisignPublicKey
	defaultChecksumMinisignPublicKey = ""
	t.Cleanup(func() {
		defaultChecksumMinisignPublicKey = previous
	})

	err := VerifyChecksumSignature(context.Background(), []byte("checksums"), []byte("signature"))
	if err == nil {
		t.Fatal("VerifyChecksumSignature() error = nil, want missing public key")
	}
	if !strings.Contains(err.Error(), "public key is not configured") {
		t.Fatalf("VerifyChecksumSignature() error = %v, want missing public key", err)
	}
}

func TestDefaultChecksumSignatureKeyParses(t *testing.T) {
	if _, _, err := parseMinisignPublicKey(defaultChecksumMinisignPublicKey); err != nil {
		t.Fatalf("parseMinisignPublicKey(defaultChecksumMinisignPublicKey) error = %v", err)
	}
}

func TestNewGitHubClientDefaultsTransportBounds(t *testing.T) {
	t.Parallel()

	client := NewGitHubClient(GitHubClientConfig{})
	if client.http == http.DefaultClient {
		t.Fatal("http client = http.DefaultClient, want bounded default client")
	}
	if client.http.Timeout != defaultHTTPClientTimeout {
		t.Fatalf("http Timeout = %v, want %v", client.http.Timeout, defaultHTTPClientTimeout)
	}
	if client.maxDownloadBytes != defaultMaxDownloadBytes {
		t.Fatalf("maxDownloadBytes = %d, want %d", client.maxDownloadBytes, defaultMaxDownloadBytes)
	}
}

func TestGitHubClientListReleasesUsesBearerToken(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"tag_name":"v1.2.3"}]`)
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(GitHubClientConfig{
		APIBase:    server.URL,
		Token:      "ghs_test",
		HTTPClient: server.Client(),
	})
	releases, err := client.ListReleases(context.Background())
	if err != nil {
		t.Fatalf("ListReleases() error = %v", err)
	}
	if len(releases) != 1 || releases[0].TagName != "v1.2.3" {
		t.Fatalf("ListReleases() = %#v, want v1.2.3", releases)
	}
	if gotAuth != "Bearer ghs_test" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
}

func TestGitHubClientListReleasesRetriesWithoutTokenOnAuthFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	var authHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if len(authHeaders) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"tag_name":"v1.2.3"}]`)
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(GitHubClientConfig{
		APIBase:    server.URL,
		Token:      "bad-token",
		HTTPClient: server.Client(),
	})
	releases, err := client.ListReleases(context.Background())
	if err != nil {
		t.Fatalf("ListReleases() error = %v", err)
	}
	if len(releases) != 1 || releases[0].TagName != "v1.2.3" {
		t.Fatalf("ListReleases() = %#v, want v1.2.3", releases)
	}
	if len(authHeaders) != 2 {
		t.Fatalf("request count = %d, want 2", len(authHeaders))
	}
	if authHeaders[0] != "Bearer bad-token" {
		t.Fatalf("first Authorization = %q, want bearer token", authHeaders[0])
	}
	if authHeaders[1] != "" {
		t.Fatalf("second Authorization = %q, want empty", authHeaders[1])
	}
}

func TestGitHubClientDownloadRejectsOversize(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abcd"))
	}))
	t.Cleanup(server.Close)

	client := NewGitHubClient(GitHubClientConfig{
		HTTPClient:       server.Client(),
		MaxDownloadBytes: 3,
	})
	_, err := client.Download(context.Background(), server.URL)
	if err == nil {
		t.Fatal("Download() error = nil, want oversize error")
	}
	if !strings.Contains(err.Error(), "exceeds maximum download size") {
		t.Fatalf("Download() error = %v, want maximum download size", err)
	}
}

func TestGitHubClientDownloadHonorsHTTPTimeout(t *testing.T) {
	t.Parallel()

	readStarted := make(chan struct{}, 1)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &contextBlockingBody{
					done:        request.Context().Done(),
					contextErr:  request.Context().Err,
					readStarted: readStarted,
				},
				Header: make(http.Header),
			}, nil
		}),
		Timeout: 100 * time.Millisecond,
	}
	client := NewGitHubClient(GitHubClientConfig{HTTPClient: httpClient})
	result := make(chan error, 1)
	go func() {
		_, err := client.Download(context.Background(), "https://example.test/release")
		result <- err
	}()

	select {
	case <-readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Download() did not start reading the response body")
	}

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Download() error = nil, want timeout")
		}
		if !strings.Contains(err.Error(), "timeout") && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Download() error = %v, want timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download() did not return after the HTTP client timeout")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type contextBlockingBody struct {
	done        <-chan struct{}
	contextErr  func() error
	readStarted chan<- struct{}
}

func (b *contextBlockingBody) Read([]byte) (int, error) {
	select {
	case b.readStarted <- struct{}{}:
	default:
	}
	<-b.done
	return 0, b.contextErr()
}

func (*contextBlockingBody) Close() error {
	return nil
}

func TestServiceAppliesHubPinnedReleaseWithMinisignSignature(t *testing.T) {
	for _, test := range []struct {
		name            string
		current         string
		follow          bool
		brew            bool
		explicit        bool
		fromRelease     bool
		hubTarget       string
		hubError        error
		invalidSelector string
	}{
		{name: "selected release", current: "1.2.3"},
		{name: "explicit release refuses wrong lock selector", current: "1.2.3", explicit: true, hubTarget: "operator-landed-a69c4b1dd060", invalidSelector: "DETENT_INSTALL_LOCK"},
		{name: "explicit release refuses wrong state selector", current: "1.2.3", explicit: true, hubTarget: "operator-landed-a69c4b1dd060", invalidSelector: "DETENT_STATE_DIR"},
		{name: "explicit release with operator Hub build", current: "1.2.3", explicit: true, hubTarget: "operator-landed-a69c4b1dd060"},
		{name: "explicit from-release with operator Hub build", current: "1.2.3", explicit: true, fromRelease: true, hubTarget: "operator-landed-a69c4b1dd060"},
		{name: "explicit release ignores older Hub pin", current: "1.2.3", explicit: true, hubTarget: "1.2.2"},
		{name: "explicit release when Hub is unavailable", current: "1.2.3", explicit: true, hubError: errors.New("Hub unavailable")},
		{name: "follow Hub below latest", current: "1.2.3", follow: true},
		{name: "runner ahead of Hub", current: "1.2.5", follow: true},
		{name: "installed development build", current: "operator-landed-abcdef123456", follow: true},
		{name: "Homebrew enrolled runner", current: "1.2.3", follow: true, brew: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmp := t.TempDir()
			binary := filepath.Join(tmp, "bin", "detent")
			lockPath := filepath.Join(tmp, "state", "install.lock")
			recoveryStatePath := filepath.Join(tmp, "state", startupRecoveryStateName)
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

			legacyHome := filepath.Join(tmp, "home")
			legacyLock := filepath.Join(legacyHome, ".detent", "install.lock")
			legacyReceipt := "binary=/other/install/detent\nversion=1.0.0\n"
			if err := os.MkdirAll(filepath.Dir(legacyLock), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(legacyLock, []byte(legacyReceipt), 0o600); err != nil {
				t.Fatal(err)
			}

			installEnv := map[string]string{"DETENT_INSTALL_LOCK": lockPath}
			if test.invalidSelector != "" {
				if err := os.WriteFile(lockPath, []byte(legacyReceipt), 0o600); err != nil {
					t.Fatal(err)
				}
				legacyReceipt = "binary=" + binary + "\n"
				if err := os.WriteFile(legacyLock, []byte(legacyReceipt), 0o600); err != nil {
					t.Fatal(err)
				}
				if test.invalidSelector == "DETENT_STATE_DIR" {
					installEnv = map[string]string{"DETENT_STATE_DIR": filepath.Dir(lockPath)}
				}
			}

			archiveName := "detent_1.2.4_linux_amd64.tar.gz"
			checksumName := "detent_1.2.4_checksums.txt"
			signatureName := checksumName + ".minisig"
			archive := detentUpdateArchive(t, "updated")
			provenanceBytes := testReleaseProvenance(t, "v1.2.4", testUpdatedCommit)
			archiveSum := sha256.Sum256(archive)
			provenanceSum := sha256.Sum256(provenanceBytes)
			checksums := fmt.Sprintf("%x  %s\n%x  %s\n", archiveSum, archiveName, provenanceSum, provenanceAssetName)
			publicKey, privateKey, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatalf("GenerateKey() error = %v", err)
			}
			keyID := []byte("12345678")
			signature := testMinisignSignature(t, privateKey, keyID, []byte(checksums), "detent checksums v1.2.4")

			previous := defaultChecksumMinisignPublicKey
			defaultChecksumMinisignPublicKey = testMinisignPublicKey(publicKey, keyID)
			t.Cleanup(func() {
				defaultChecksumMinisignPublicKey = previous
			})

			const releaseURL = "https://releases.example.test"
			release := Release{TagName: "v1.2.4", Assets: []Asset{
				{Name: archiveName, BrowserDownloadURL: releaseURL + "/archive"},
				{Name: checksumName, BrowserDownloadURL: releaseURL + "/checksums"},
				{Name: signatureName, BrowserDownloadURL: releaseURL + "/checksums.minisig"},
				{Name: provenanceAssetName, BrowserDownloadURL: releaseURL + "/provenance"},
			}}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases":
					if err := json.NewEncoder(w).Encode([]Release{release}); err != nil {
						t.Fatal(err)
					}
				case "/releases/tags/v1.2.4":
					if err := json.NewEncoder(w).Encode(release); err != nil {
						t.Fatal(err)
					}
				case "/archive":
					_, _ = w.Write(archive)
				case "/checksums":
					fmt.Fprint(w, checksums)
				case "/checksums.minisig":
					_, _ = w.Write(signature)
				case "/provenance":
					_, _ = w.Write(provenanceBytes)
				default:
					http.NotFound(w, r)
				}
			})
			httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				return recorder.Result(), nil
			})}

			service := NewService(Config{
				CurrentVersion: test.current,
				HomeDir:        legacyHome,
				CurrentCommit:  testPreviousCommit,
				ExecutablePath: binary,
				GOOS:           "linux",
				GOARCH:         "amd64",
				Client: NewGitHubClient(GitHubClientConfig{
					APIBase:    releaseURL,
					HTTPClient: httpClient,
				}),
				Env: installEnv,
				BinaryVerifier: func(context.Context, string) (string, error) {
					return "version: v1.2.4\ncommit: " + testUpdatedCommit + "\n", nil
				},
			})

			if !test.follow {
				if _, err := service.Check(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				service.cfg.TargetVersion = func(context.Context) (string, error) { return "1.2.99", nil }
			}
			if test.brew {
				service.cfg.EvalSymlinks = func(string) (string, error) { return "/opt/homebrew/Cellar/detent/1.2.3/bin/detent", nil }
			}
			if !test.follow {
				service.cfg.TargetVersion = func(context.Context) (string, error) { return "v1.2.4", nil }
			}
			if !test.follow && !test.explicit {
				stale, staleErr := service.Apply(t.Context(), ApplyOptions{AssumeYes: true, ExpectedVersion: "1.2.5"})
				if !errors.Is(staleErr, ErrRefused) || stale.Action != ActionRefused {
					t.Fatalf("changed selected release = %+v, %v", stale, staleErr)
				}
			}
			var preflightPath string
			applyOptions := ApplyOptions{
				AssumeYes:         true,
				FollowHub:         test.follow,
				FromRelease:       test.follow,
				ExpectedVersion:   "1.2.4",
				RecoveryStatePath: recoveryStatePath,
				Preflight: func(_ context.Context, path string) error {
					preflightPath = path
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if strings.TrimSpace(string(raw)) != "updated" {
						return fmt.Errorf("candidate content = %q", raw)
					}
					return nil
				},
			}
			var status Status
			if test.explicit {
				service.cfg.TargetVersion = func(context.Context) (string, error) { return test.hubTarget, test.hubError }
				drains, restarts, releases := 0, 0, 0
				scheduler, schedulerErr := NewScheduler(SchedulerConfig{
					CheckInterval: time.Hour, Updater: service, ApplyOptions: applyOptions,
					ReserveDrain: func(context.Context) (func(), error) {
						drains++
						return func() { releases++ }, nil
					},
					RequestRestart: func(path string) bool {
						if path != binary || releases != 0 {
							t.Errorf("restart path = %q, released drains = %d", path, releases)
						}
						restarts++
						return true
					},
				})
				if schedulerErr != nil {
					t.Fatal(schedulerErr)
				}
				status, err = scheduler.ApplyRelease(t.Context(), test.fromRelease)
				if test.invalidSelector != "" {
					selectedRaw, selectedErr := os.ReadFile(lockPath)
					defaultRaw, defaultErr := os.ReadFile(legacyLock)
					binaryRaw, binaryErr := os.ReadFile(binary)
					if !errors.Is(err, ErrRefused) || status.Action != ActionRefused || drains != 1 || restarts != 0 || releases != 1 || selectedErr != nil || defaultErr != nil || binaryErr != nil || string(selectedRaw) != "binary=/other/install/detent\nversion=1.0.0\n" || string(defaultRaw) != legacyReceipt || string(binaryRaw) != "old" {
						t.Fatalf("wrong selector update = %+v, error = %v, drains/restarts/releases = %d/%d/%d, selected receipt = %q, default receipt = %q, binary = %q", status, err, drains, restarts, releases, selectedRaw, defaultRaw, binaryRaw)
					}
					return
				}
				if err == nil && (drains != 1 || restarts != 1 || releases != 0) {
					t.Fatalf("drains = %d, restarts = %d, releases = %d", drains, restarts, releases)
				}
			} else {
				status, err = service.Apply(t.Context(), applyOptions)
			}
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if status.Action != ActionUpdated {
				t.Fatalf("Action = %q, want %q", status.Action, ActionUpdated)
			}
			if !status.UpdateAvailable {
				t.Fatal("UpdateAvailable = false, want true")
			}
			expectedBinarySum := sha256.Sum256([]byte("updated"))
			if status.BinarySHA256 != hex.EncodeToString(expectedBinarySum[:]) || !status.VerifiedRelease || status.LatestCommit != testUpdatedCommit {
				t.Fatalf("applied provenance = %+v", status)
			}
			raw, err := os.ReadFile(binary)
			if err != nil {
				t.Fatalf("ReadFile(binary) error = %v", err)
			}
			if strings.TrimSpace(string(raw)) != "updated" {
				t.Fatalf("updated binary = %q, want updated", raw)
			}
			if preflightPath == "" || preflightPath == binary {
				t.Fatalf("preflight path = %q, want staged candidate", preflightPath)
			}
			previousPath := PreviousBinaryPath(binary)
			previousRaw, err := os.ReadFile(previousPath)
			if err != nil {
				t.Fatalf("ReadFile(previous) error = %v", err)
			}
			if string(previousRaw) != "old" {
				t.Fatalf("previous binary = %q, want old", previousRaw)
			}
			recoveryState := readTestStartupRecoveryState(t, recoveryStatePath)
			if recoveryState.PendingUpdate == nil {
				t.Fatal("PendingUpdate = nil, want rollback metadata")
			}
			if got := recoveryState.PendingUpdate; got.FromVersion != test.current || got.FromCommit != testPreviousCommit || got.ToVersion != "1.2.4" || got.ToCommit != testUpdatedCommit || got.PreviousBinaryPath != previousPath {
				t.Fatalf("PendingUpdate = %#v, want 1.2.3 to 1.2.4 with previous binary", got)
			}
			if got := recoveryState.PendingUpdate; got.InstallLockPath != lockPath || !got.PreviousInstallLockFound || got.PreviousInstallLock != "binary="+binary+"\n" {
				t.Fatalf("PendingUpdate install lock = %#v, want preserved pre-update metadata", got)
			}
			if got := InstalledReleaseVersion(DetectionOptions{
				ExecutablePath: binary,
				GOOS:           "linux",
				Env:            map[string]string{"DETENT_INSTALL_LOCK": lockPath},
			}); got != "1.2.4" {
				t.Fatalf("InstalledReleaseVersion() = %q, want 1.2.4", got)
			}
			legacyRaw, legacyErr := os.ReadFile(legacyLock)
			if legacyErr != nil || string(legacyRaw) != legacyReceipt {
				t.Fatalf("other installation receipt = %q, error = %v", legacyRaw, legacyErr)
			}
			metadata, ok := readInstallLock(lockPath)
			if !ok || metadata.commit != testUpdatedCommit {
				t.Fatalf("install lock = %#v, found = %t, want updated full commit", metadata, ok)
			}
		})
	}
}

func TestServiceRejectsReleaseUpdateWithoutChecksumSignature(t *testing.T) {
	previous := defaultChecksumMinisignPublicKey
	defaultChecksumMinisignPublicKey = ""
	t.Cleanup(func() {
		defaultChecksumMinisignPublicKey = previous
	})

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
			downloads: map[string][]byte{
				"https://example.invalid/archive":   archive,
				"https://example.invalid/checksums": []byte(checksums),
			},
		},
		Env: map[string]string{"DETENT_INSTALL_LOCK": lockPath},
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v1.2.4\n", nil
		},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{AssumeYes: true})
	if err == nil || !strings.Contains(err.Error(), "does not include a minisign signature") {
		t.Fatalf("Apply() error = %v, want missing signature", err)
	}
	if status.Action != ActionRefused {
		t.Fatalf("Action = %q, want %q", status.Action, ActionRefused)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if string(raw) != "old" {
		t.Fatalf("binary = %q, want original", raw)
	}
}

func TestServiceRejectsBadChecksumSignatureBeforeReplacement(t *testing.T) {
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
	signatureName := checksumName + ".minisig"
	archive := detentUpdateArchive(t, "updated")
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%x  %s\n", sum, archiveName)

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
					{Name: signatureName, BrowserDownloadURL: "https://example.invalid/checksums.minisig"},
					{Name: provenanceAssetName, BrowserDownloadURL: "https://example.invalid/provenance"},
				},
			}},
			downloads: map[string][]byte{
				"https://example.invalid/archive":           archive,
				"https://example.invalid/checksums":         []byte(checksums),
				"https://example.invalid/checksums.minisig": []byte("bad signature"),
			},
		},
		Env: map[string]string{"DETENT_INSTALL_LOCK": lockPath},
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v1.2.4\n", nil
		},
		ChecksumSignatureVerifier: func(context.Context, []byte, []byte) error {
			return errors.New("bad checksum signature")
		},
	})

	status, err := service.Apply(context.Background(), ApplyOptions{AssumeYes: true})
	if err == nil {
		t.Fatal("Apply() error = nil, want bad checksum signature")
	}
	if status.Action != ActionRefused {
		t.Fatalf("Action = %q, want %q", status.Action, ActionRefused)
	}
	if !strings.Contains(status.Message, "bad checksum signature") {
		t.Fatalf("Message = %q, want bad checksum signature", status.Message)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if string(raw) != "old" {
		t.Fatalf("binary = %q, want original binary", raw)
	}
}

func TestServiceRejectsUntrustedReleaseIdentityBeforeReplacement(t *testing.T) {
	t.Parallel()

	validProvenance := testReleaseProvenance(t, "v1.2.4", testUpdatedCommit)
	skippedProvenance, err := provenance.Marshal(provenance.Manifest{
		Schema:     provenance.Schema,
		Repository: releaseRepository,
		Tag:        "v1.2.4",
		Commit:     testUpdatedCommit,
		Checks: []provenance.Check{{
			Name:       "CI",
			Status:     "completed",
			Conclusion: "skipped",
			CheckRunID: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Marshal(skipped provenance) error = %v", err)
	}

	tests := []struct {
		name            string
		provenance      []byte
		checksummed     []byte
		candidateCommit string
		wantErr         string
	}{
		{
			name:            "tampered provenance",
			provenance:      append(append([]byte(nil), validProvenance...), ' '),
			checksummed:     validProvenance,
			candidateCommit: testUpdatedCommit,
			wantErr:         "checksum mismatch",
		},
		{
			name:            "skipped mandatory check",
			provenance:      skippedProvenance,
			checksummed:     skippedProvenance,
			candidateCommit: testUpdatedCommit,
			wantErr:         "skipped",
		},
		{
			name:            "same version from wrong commit",
			provenance:      validProvenance,
			checksummed:     validProvenance,
			candidateCommit: testPreviousCommit,
			wantErr:         "does not match tested release commit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			binary := filepath.Join(dir, "detent")
			lockPath := filepath.Join(dir, "install.lock")
			if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
				t.Fatalf("WriteFile(binary) error = %v", err)
			}
			if err := os.WriteFile(lockPath, []byte("binary="+binary+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile(lock) error = %v", err)
			}
			archiveName := "detent_1.2.4_linux_amd64.tar.gz"
			checksumName := "detent_1.2.4_checksums.txt"
			signatureName := checksumName + ".minisig"
			archive := detentUpdateArchive(t, "candidate")
			archiveSum := sha256.Sum256(archive)
			provenanceSum := sha256.Sum256(tt.checksummed)
			checksums := fmt.Appendf(nil, "%x  %s\n%x  %s\n", archiveSum, archiveName, provenanceSum, provenanceAssetName)
			service := NewService(Config{
				CurrentVersion: "1.2.3",
				CurrentCommit:  testPreviousCommit,
				ExecutablePath: binary,
				GOOS:           "linux",
				GOARCH:         "amd64",
				Client: staticReleaseClient{
					releases: []Release{{
						TagName: "v1.2.4",
						Assets: []Asset{
							{Name: archiveName, BrowserDownloadURL: "archive"},
							{Name: checksumName, BrowserDownloadURL: "checksums"},
							{Name: signatureName, BrowserDownloadURL: "signature"},
							{Name: provenanceAssetName, BrowserDownloadURL: "provenance"},
						},
					}},
					downloads: map[string][]byte{
						"archive":    archive,
						"checksums":  checksums,
						"signature":  []byte("valid signature"),
						"provenance": tt.provenance,
					},
				},
				Env: map[string]string{"DETENT_INSTALL_LOCK": lockPath},
				BinaryVerifier: func(context.Context, string) (string, error) {
					return "version: v1.2.4\ncommit: " + tt.candidateCommit + "\n", nil
				},
				ChecksumSignatureVerifier: acceptChecksumSignature,
			})

			status, err := service.Apply(context.Background(), ApplyOptions{AssumeYes: true})
			if err == nil || !strings.Contains(status.Message, tt.wantErr) {
				t.Fatalf("Apply() error = %v, message = %q, want containing %q", err, status.Message, tt.wantErr)
			}
			if status.Action != ActionRefused {
				t.Fatalf("Action = %q, want %q", status.Action, ActionRefused)
			}
			raw, readErr := os.ReadFile(binary)
			if readErr != nil {
				t.Fatalf("ReadFile(binary) error = %v", readErr)
			}
			if string(raw) != "old" {
				t.Fatalf("binary = %q, want original", raw)
			}
		})
	}
}

func TestServiceCheckReportsCriticalReleaseMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		critical bool
	}{
		{name: "ordinary release", body: "Routine fixes"},
		{name: "critical release", body: "Burn brake release\n\n[DETENT-CRITICAL]", critical: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			service := NewService(Config{
				CurrentVersion: "1.2.3",
				ExecutablePath: "/opt/detent/bin/detent",
				GOOS:           "linux",
				GOARCH:         "amd64",
				Client: staticReleaseClient{releases: []Release{{
					TagName: "v1.2.4",
					Body:    tt.body,
				}}},
			})
			status, err := service.Check(context.Background())
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if status.Critical != tt.critical {
				t.Fatalf("Check().Critical = %t, want %t", status.Critical, tt.critical)
			}
		})
	}
}

func TestServiceChoosesHubUpdateTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		target      string
		hubError    error
		release     string
		draft       bool
		wantTag     string
		available   bool
		wantError   bool
		wantPath    string
		applyTarget string
		applyError  error
		urgent      bool
	}{
		{name: "Hub pinned below latest", target: "1.2.4", release: "v1.2.4", wantTag: "v1.2.4", available: true, wantPath: "/releases/tags/v1.2.4"},
		{name: "Hub older than runner", target: "v1.2.2", wantTag: "v1.2.2"},
		{name: "Hub matches runner", target: "v1.2.3", wantTag: "v1.2.3"},
		{name: "Hub unreachable", hubError: errors.New("Hub unavailable"), wantError: true},
		{name: "Hub missing version", wantError: true},
		{name: "Hub development version", target: "dev", wantError: true},
		{name: "Hub pin has no release", target: "v1.2.4", wantError: true, wantPath: "/releases/tags/v1.2.4"},
		{name: "Hub pin resolves to wrong release", target: "v1.2.4", release: "v1.2.5", wantError: true, wantPath: "/releases/tags/v1.2.4"},
		{name: "Hub pin is draft", target: "v1.2.4", release: "v1.2.4", draft: true, wantError: true, wantPath: "/releases/tags/v1.2.4"},
		{name: "Hub explicitly pins prerelease", target: "v1.2.4-rc.1", release: "v1.2.4-rc.1", wantTag: "v1.2.4-rc.1", available: true, wantPath: "/releases/tags/v1.2.4-rc.1"},
		{name: "Hub rolls back before apply", target: "v1.2.4", release: "v1.2.4", wantTag: "v1.2.4", available: true, wantPath: "/releases/tags/v1.2.4", applyTarget: "v1.2.2"},
		{name: "Hub unreachable before apply", target: "v1.2.4", release: "v1.2.4", wantTag: "v1.2.4", available: true, wantPath: "/releases/tags/v1.2.4", applyError: errors.New("Hub unavailable before apply")},
		{name: "urgent target survives Hub advancing during drain", target: "v1.2.4", release: "v1.2.4", wantTag: "v1.2.4", available: true, wantPath: "/releases/tags/v1.2.4", applyTarget: "v1.2.5", urgent: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var paths []string
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				var output any = Release{TagName: test.release, Draft: test.draft, Prerelease: strings.Contains(test.release, "-rc.")}
				status := http.StatusOK
				if r.URL.Path == "/releases" {
					output = []Release{{TagName: "v9.0.0"}}
				} else if test.release == "" {
					status = http.StatusNotFound
				}
				payload, err := json.Marshal(output)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)}, nil
			})}
			target, hubError := test.target, test.hubError
			service := NewService(Config{
				CurrentVersion: "1.2.3",
				ExecutablePath: "/opt/detent/bin/detent",
				GOOS:           "linux",
				GOARCH:         "amd64",
				Client:         NewGitHubClient(GitHubClientConfig{APIBase: "https://releases.example.test", HTTPClient: client}),
				TargetVersion:  func(context.Context) (string, error) { return target, hubError },
			})
			status, err := service.Check(t.Context())
			if (err != nil) != test.wantError {
				t.Fatalf("Check() error = %v, want error = %t", err, test.wantError)
			}
			if status.LatestTag != test.wantTag || status.UpdateAvailable != test.available {
				t.Fatalf("Check() = %#v, want tag %q, available %t", status, test.wantTag, test.available)
			}
			if test.wantPath == "" && len(paths) != 0 || test.wantPath != "" && (len(paths) != 1 || paths[0] != test.wantPath) {
				t.Fatalf("release requests = %v, want only %q", paths, test.wantPath)
			}
			if test.hubError != nil && !errors.Is(err, test.hubError) {
				t.Fatalf("Check() lost Hub error: %v", err)
			}
			if test.applyTarget != "" || test.applyError != nil {
				target, hubError = test.applyTarget, test.applyError
				if test.urgent {
					selected, err := service.Apply(t.Context(), ApplyOptions{AssumeYes: true, ExpectedVersion: "1.2.4", Urgent: true})
					if !errors.Is(err, ErrRefused) || selected.LatestTag != "v1.2.4" || len(paths) != 2 || paths[1] != test.wantPath {
						t.Fatalf("urgent target changed during drain: %+v, %v, requests=%v", selected, err, paths)
					}
					return
				}
				applied, err := service.Apply(t.Context(), ApplyOptions{AssumeYes: true})
				if !errors.Is(err, test.applyError) || applied.UpdateAvailable || len(paths) != 1 {
					t.Fatalf("Apply() reused obsolete target: status %#v, error %v, release requests %v", applied, err, paths)
				}
				if test.applyError == nil && (applied.Action != ActionUpToDate || applied.LatestTag != test.applyTarget) {
					t.Fatalf("Apply() = %#v, want up to date at Hub target %s", applied, test.applyTarget)
				}
			}
		})
	}
}

func TestServiceGoInstallUsesPreparedSourceGuidance(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		current string
		tag     string
	}{
		{current: "1.2.3", tag: "v1.2.4"},
		{current: "1.3.0-rc.1", tag: "v1.3.0-rc.2"},
	} {
		t.Run(tt.tag, func(t *testing.T) {
			t.Parallel()
			goBin := t.TempDir()
			binary := filepath.Join(goBin, "detent")
			if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			service := NewService(Config{
				CurrentVersion: tt.current,
				ExecutablePath: binary,
				GOOS:           "linux",
				GOARCH:         "amd64",
				Client:         staticReleaseClient{releases: []Release{{TagName: tt.tag, Prerelease: strings.Contains(tt.tag, "-")}}},
				Env:            map[string]string{"GOBIN": goBin},
				BinaryVerifier: func(context.Context, string) (string, error) {
					t.Fatal("attempted to replace the Go-managed binary")
					return "", nil
				},
			})
			status, err := service.Apply(context.Background(), ApplyOptions{AssumeYes: true})
			if !errors.Is(err, ErrRefused) || status.Action != ActionRefused {
				t.Fatalf("Apply() = %+v, %v", status, err)
			}
			if status.Command != sourceUpdateCommand || !strings.Contains(status.Message, "prepared release source archive") {
				t.Fatalf("missing supported build guidance: %+v", status)
			}
			data, err := os.ReadFile(binary)
			if err != nil || string(data) != "old" {
				t.Fatalf("original binary changed: %q, %v", data, err)
			}
		})
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
	if status.Command != sourceUpdateCommand {
		t.Fatalf("Command = %q, want %q", status.Command, sourceUpdateCommand)
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
	signatureName := checksumName + ".minisig"
	archive := detentUpdateArchive(t, "updated")
	provenanceBytes := testReleaseProvenance(t, "v1.2.4", testUpdatedCommit)
	archiveSum := sha256.Sum256(archive)
	provenanceSum := sha256.Sum256(provenanceBytes)
	checksums := fmt.Sprintf("%x  %s\n%x  %s\n", archiveSum, archiveName, provenanceSum, provenanceAssetName)
	downloads := map[string][]byte{
		"https://example.invalid/archive":           archive,
		"https://example.invalid/checksums":         []byte(checksums),
		"https://example.invalid/checksums.minisig": []byte("valid signature"),
		"https://example.invalid/provenance":        provenanceBytes,
	}
	var stderr bytes.Buffer
	var verifiedPath string
	service := NewService(Config{
		CurrentVersion: "1.2.3",
		CurrentCommit:  testPreviousCommit,
		ExecutablePath: binary,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: staticReleaseClient{
			releases: []Release{{
				TagName: "v1.2.4",
				Assets: []Asset{
					{Name: archiveName, BrowserDownloadURL: "https://example.invalid/archive"},
					{Name: checksumName, BrowserDownloadURL: "https://example.invalid/checksums"},
					{Name: signatureName, BrowserDownloadURL: "https://example.invalid/checksums.minisig"},
					{Name: provenanceAssetName, BrowserDownloadURL: "https://example.invalid/provenance"},
				},
			}},
			downloads: downloads,
		},
		Env:     map[string]string{"GOBIN": goBin},
		HomeDir: tmp,
		BinaryVerifier: func(_ context.Context, path string) (string, error) {
			verifiedPath = path
			return "version: v1.2.4\ncommit: " + testUpdatedCommit + "\n", nil
		},
		ChecksumSignatureVerifier: acceptChecksumSignature,
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

	lockPath := filepath.Join(tmp, ".detent", "install.lock")
	lockRaw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("ReadFile(lock) error = %v", err)
	}
	lockText := string(lockRaw)
	if !strings.Contains(lockText, "binary="+binary+"\n") {
		t.Fatalf("install lock = %q, want binary path", lockText)
	}
	if !strings.Contains(lockText, "installed_at=") {
		t.Fatalf("install lock = %q, want installed_at", lockText)
	}
	if !strings.Contains(lockText, "version=1.2.4\n") {
		t.Fatalf("install lock = %q, want applied version", lockText)
	}
	if got := InstalledReleaseVersion(DetectionOptions{
		ExecutablePath: binary,
		GOOS:           "linux",
		HomeDir:        tmp,
	}); got != "1.2.4" {
		t.Fatalf("InstalledReleaseVersion() = %q, want 1.2.4", got)
	}

	detected := DetectInstallSource(DetectionOptions{
		CurrentVersion: "1.2.4",
		ExecutablePath: binary,
		GOOS:           "linux",
		HomeDir:        tmp,
		Env:            map[string]string{"GOBIN": goBin},
	})
	if detected.Source != InstallSourceRelease {
		t.Fatalf("Source after release swap = %q, want %q", detected.Source, InstallSourceRelease)
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
	err := ReplaceBinary(context.Background(), Replacement{
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
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("Stat(target) error = %v", err)
		}
		if got := info.Mode().Perm(); got != 0o750 {
			t.Fatalf("mode = %v, want 0750", got)
		}
	}
}

func TestReplaceBinaryRollsBackWhenVerificationFails(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	err := ReplaceBinary(context.Background(), Replacement{
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

func TestReplaceBinaryRollsBackWhenAfterReplaceFails(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	err := ReplaceBinary(context.Background(), Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "linux",
		Verify: func(context.Context, string) (string, error) {
			return "version: v1.2.4\n", nil
		},
		AfterReplace: func(context.Context, string) error {
			return errors.New("metadata failed")
		},
	})
	if err == nil {
		t.Fatal("ReplaceBinary() error = nil, want metadata failure")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(target) error = %v", err)
	}
	if string(raw) != "old" {
		t.Fatalf("target = %q, want rollback to old", raw)
	}
}

func TestReplaceBinarySignsUnsignedDarwinBeforeVerify(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	var calls []string
	err := ReplaceBinary(context.Background(), Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "darwin",
		CodeSignatureVerifier: func(_ context.Context, path string) (bool, error) {
			calls = append(calls, "signature:"+path)
			return false, nil
		},
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
	want := []string{"signature:" + target, "sign:" + target, "verify:" + target}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
}

func TestReplaceBinaryPreservesValidDarwinSignature(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	target := filepath.Join(tmp, "detent")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}

	var calls []string
	err := ReplaceBinary(context.Background(), Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "darwin",
		CodeSignatureVerifier: func(_ context.Context, path string) (bool, error) {
			calls = append(calls, "signature:"+path)
			return true, nil
		},
		Sign: func(context.Context, string) error {
			t.Fatal("Sign called for a binary with a valid code signature")
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
	want := []string{"signature:" + target, "verify:" + target}
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
	err := ReplaceBinary(context.Background(), Replacement{
		Target: target,
		Binary: []byte("new"),
		Mode:   0o755,
		GOOS:   "windows",
		StartProcess: func(_ context.Context, command string, args []string) error {
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

func testMinisignPublicKey(publicKey ed25519.PublicKey, keyID []byte) string {
	packet := append([]byte("Ed"), keyID...)
	packet = append(packet, publicKey...)
	return base64.StdEncoding.EncodeToString(packet)
}

func testReleaseProvenance(t *testing.T, tag string, commit string) []byte {
	t.Helper()
	raw, err := provenance.Marshal(provenance.Manifest{
		Schema:     provenance.Schema,
		Repository: releaseRepository,
		Tag:        tag,
		Commit:     commit,
		Checks: []provenance.Check{{
			Name:       "CI",
			Status:     "completed",
			Conclusion: "success",
			CheckRunID: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Marshal(release provenance) error = %v", err)
	}
	return raw
}

func testMinisignSignature(t *testing.T, privateKey ed25519.PrivateKey, keyID []byte, message []byte, trustedComment string) []byte {
	t.Helper()

	digest := blake2b.Sum512(message)
	signature := ed25519.Sign(privateKey, digest[:])
	packet := append([]byte("ED"), keyID...)
	packet = append(packet, signature...)
	globalSignature := ed25519.Sign(privateKey, append(signature, []byte(trustedComment)...))
	return fmt.Appendf(nil,
		"untrusted comment: signature from minisign secret key\n%s\ntrusted comment: %s\n%s\n",
		base64.StdEncoding.EncodeToString(packet),
		trustedComment,
		base64.StdEncoding.EncodeToString(globalSignature),
	)
}

func acceptChecksumSignature(context.Context, []byte, []byte) error {
	return nil
}

type staticReleaseClient struct {
	releases  []Release
	downloads map[string][]byte
}

func (c staticReleaseClient) ListReleases(context.Context) ([]Release, error) {
	return c.releases, nil
}

func (c staticReleaseClient) GetRelease(_ context.Context, tag string) (Release, error) {
	for _, release := range c.releases {
		if release.TagName == tag {
			return release, nil
		}
	}
	return Release{}, errors.New("release not found")
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

func TestExplicitInstallerReceiptDoesNotFallBackToOtherOwners(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		goos      string
		goInstall bool
	}{
		{name: "Windows installer directory", goos: "windows"},
		{name: "Go install directory", goos: "linux", goInstall: true},
		{name: "macOS custom release directory", goos: "darwin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			home := filepath.Join(tmp, "home")
			binDir := filepath.Join(home, ".detent", "bin")
			name := "detent"
			if test.goInstall {
				binDir = filepath.Join(tmp, "gobin")
			}
			if test.goos == "windows" {
				name = "detent.exe"
			}
			binary := filepath.Join(binDir, name)
			defaultLock := filepath.Join(home, ".detent", "install.lock")
			if err := os.MkdirAll(filepath.Dir(defaultLock), 0o755); err != nil {
				t.Fatal(err)
			}
			defaultOwner := binary
			if test.goInstall || test.goos == "windows" {
				defaultOwner = "/other/detent"
			}
			if err := os.WriteFile(defaultLock, []byte("binary="+defaultOwner+"\nversion=1.2.3\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			wrongLock := filepath.Join(tmp, "other-install.lock")
			if err := os.WriteFile(wrongLock, []byte("binary=/other/detent\nversion=1.0.0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			opts := DetectionOptions{CurrentVersion: "1.2.3", ExecutablePath: binary, HomeDir: home, GOOS: test.goos, Env: map[string]string{"DETENT_INSTALL_LOCK": wrongLock, "DETENT_STATE_DIR": filepath.Dir(defaultLock), "GOBIN": binDir}}
			if got := DetectInstallSource(opts); got.Source != InstallSourceUnknown {
				t.Fatalf("explicit wrong receipt owner = %+v", got)
			}
			if got := InstalledReleaseVersion(opts); got != "" {
				t.Fatalf("version from another receipt = %q", got)
			}
		})
	}
}
