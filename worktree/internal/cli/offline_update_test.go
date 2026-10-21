package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/blake2b"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const offlineUpdateVersion = "1.2.3"
const offlineUpdateCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestOfflineCLIUpdateReportsVerifiedController(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement completes in a detached process")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "selected.yaml")
	if err := os.WriteFile(configPath, []byte("apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 2\n  scheduling: weighted\nupdate:\n  auto_check_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETENT_INSTALL_LOCK", filepath.Join(dir, "install.lock"))
	t.Setenv("DETENT_OFFLINE_UPDATE_CONFIG", configPath)
	t.Setenv("DETENT_OFFLINE_UPDATE_REPORT", filepath.Join(dir, "heartbeat.json"))
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyID := []byte("12345678")
	publicKey := base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), keyID...), public...))
	candidate := filepath.Join(dir, "candidate")
	buildCtx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "test", "-c", "-p", "4", "-ldflags", "-X github.com/digitaldrywood/detent/internal/update.defaultChecksumMinisignPublicKey="+publicKey, "-o", candidate, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build controller fixture: %v\n%s", err, output)
	}
	payload, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "detent", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := provenance.Marshal(provenance.Manifest{Schema: provenance.Schema, Repository: "digitaldrywood/detent", Tag: "v" + offlineUpdateVersion, Commit: offlineUpdateCommit,
		Checks: []provenance.Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("detent_%s_%s_%s.tar.gz", offlineUpdateVersion, runtime.GOOS, runtime.GOARCH)
	checksumName := "detent_" + offlineUpdateVersion + "_checksums.txt"
	checksums := fmt.Appendf(nil, "%x  %s\n%x  detent_release_provenance.json\n", sha256.Sum256(archive.Bytes()), archiveName, sha256.Sum256(manifest))
	digest := blake2b.Sum512(checksums)
	messageSignature := ed25519.Sign(private, digest[:])
	packet := append(append([]byte("ED"), keyID...), messageSignature...)
	trusted := "offline fixture"
	signature := fmt.Appendf(nil, "untrusted comment: test\n%s\ntrusted comment: %s\n%s\n", base64.StdEncoding.EncodeToString(packet), trusted,
		base64.StdEncoding.EncodeToString(ed25519.Sign(private, append(messageSignature, []byte(trusted)...))))
	client := offlineReleaseClient{downloads: map[string][]byte{archiveName: archive.Bytes(), checksumName: checksums, checksumName + ".minisig": signature, "detent_release_provenance.json": manifest}}
	for name := range client.downloads {
		client.release.Assets = append(client.release.Assets, detentupdate.Asset{Name: name, BrowserDownloadURL: name})
	}
	client.release.TagName = "v" + offlineUpdateVersion
	binary := filepath.Join(dir, "detent")
	if err := os.WriteFile(binary, []byte("unverified old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DETENT_INSTALL_LOCK"), []byte("binary="+binary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := detentupdate.NewService(detentupdate.Config{CurrentVersion: offlineUpdateVersion, CurrentCommit: offlineUpdateCommit, ExecutablePath: binary, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Client: client,
		ChecksumSignatureVerifier: func(_ context.Context, checksums, signature []byte) error {
			return detentupdate.VerifyMinisignSignature(publicKey, checksums, signature)
		},
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v" + offlineUpdateVersion + "\ncommit: " + offlineUpdateCommit + "\n", nil
		},
	})
	options := detentupdate.ApplyOptions{AssumeYes: true, FromRelease: true, RecoveryStatePath: detentupdate.RecoveryStatePath(configPath),
		Preflight: detentupdate.CommandPreflight("-test.run=^TestOfflineUpdateControllerProcess$"),
	}
	command := &cobra.Command{}
	command.Flags().String("config", configPath, "")
	if _, handled, err := ApplyRunningUpdate(t.Context(), command, options); err != nil || handled {
		t.Fatalf("offline CLI coordination: handled=%t err=%v", handled, err)
	}
	t.Setenv("DETENT_OFFLINE_UPDATE_PHASE", "preflight")
	applied, err := service.Apply(t.Context(), options)
	if err != nil || !applied.VerifiedRelease || applied.Action != detentupdate.ActionUpdated {
		t.Fatalf("offline CLI apply: %+v %v", applied, err)
	}
	if _, err := os.Stat(options.RecoveryStatePath); err != nil {
		t.Fatalf("selected config lost startup recovery: %v", err)
	}
	t.Setenv("DETENT_OFFLINE_UPDATE_PHASE", "heartbeat")
	child := exec.CommandContext(t.Context(), binary, "-test.run=^TestOfflineUpdateControllerProcess$")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh controller: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(os.Getenv("DETENT_OFFLINE_UPDATE_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	var heartbeat runnerauth.UpdateObservation
	if err := json.Unmarshal(raw, &heartbeat); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if heartbeat.Validate() != nil || !heartbeat.Running.VerifiedRelease || heartbeat.Running.Source != "release" || heartbeat.Running.Version != offlineUpdateVersion || heartbeat.Running.Commit != offlineUpdateCommit || heartbeat.Running.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("offline evidence did not reach fresh heartbeat: %+v", heartbeat)
	}
}

func TestOfflineUpdateControllerProcess(t *testing.T) {
	phase := os.Getenv("DETENT_OFFLINE_UPDATE_PHASE")
	if phase == "" {
		t.Skip("controller subprocess fixture")
	}
	global, err := globalconfig.Read(os.Getenv("DETENT_OFFLINE_UPDATE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if phase == "preflight" {
		return
	}
	cfg := BootConfig{Version: offlineUpdateVersion, Build: buildinfo.Info{Version: offlineUpdateVersion, Commit: offlineUpdateCommit}, Global: global, Shutdown: NewShutdownController(), Restart: NewRestartRequest()}
	scheduler, err := newRuntimeUpdateScheduler(t.Context(), cfg, nil, func(context.Context) (func(), bool) { return func() {}, true }, func(context.Context) (func(), error) {
		t.Fatal("offline repair unexpectedly drained")
		return nil, detentupdate.ErrRefused
	})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := enrolledUpdateOwner(t.Context(), scheduler, scheduler.RunningBuild())(t.Context(), nil)
	raw, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DETENT_OFFLINE_UPDATE_REPORT"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

type offlineReleaseClient struct {
	release   detentupdate.Release
	downloads map[string][]byte
}

func (c offlineReleaseClient) ListReleases(context.Context) ([]detentupdate.Release, error) {
	return []detentupdate.Release{c.release}, nil
}

func (c offlineReleaseClient) GetRelease(context.Context, string) (detentupdate.Release, error) {
	return c.release, nil
}

func (c offlineReleaseClient) Download(_ context.Context, url string) ([]byte, error) {
	data, ok := c.downloads[url]
	if !ok {
		return nil, fmt.Errorf("missing fixture asset %s", strings.TrimSpace(url))
	}
	return data, nil
}
