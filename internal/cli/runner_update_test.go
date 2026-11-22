package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestRunnerRunningBuildProvenance(t *testing.T) {
	for _, test := range []struct {
		name    string
		info    buildinfo.Info
		receipt bool
	}{
		{name: "private patched source", info: buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40), Dirty: true}},
		{name: "version is not release approval", info: buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40)}},
		{name: "forged marker cannot verify release", info: buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40)}, receipt: true},
		{name: "patched source cannot use release marker", info: buildinfo.Info{Version: "v1.2.3", Commit: strings.Repeat("a", 40), Dirty: true}, receipt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock := filepath.Join(t.TempDir(), "install.lock")
			t.Setenv("DETENT_INSTALL_LOCK", lock)
			if test.receipt {
				path, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				file, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				_, readErr := io.Copy(hash, file)
				closeErr := file.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("hash executable: %v %v", readErr, closeErr)
				}
				raw := fmt.Sprintf("binary=%s\nversion=%s\ncommit=%s\nsha256=%s\n", path, test.info.Version, test.info.Commit, hex.EncodeToString(hash.Sum(nil)))
				if err := os.WriteFile(lock, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			evidence := runnerRunningBuild(t.Context(), test.info, "fallback")
			if evidence.Validate() != nil || evidence.Version != test.info.Version || evidence.Commit != test.info.Commit || evidence.OS != runtime.GOOS || evidence.Architecture != runtime.GOARCH || evidence.VerifiedRelease {
				t.Fatalf("running build=%+v", evidence)
			}
			if test.info.Dirty && evidence.Source != "private_patched_source" {
				t.Fatalf("lost private composition=%+v", evidence)
			}
			raw, err := json.Marshal(evidence)
			if err != nil || strings.Contains(string(raw), "/Users/") || strings.Contains(string(raw), "/private/") {
				t.Fatalf("private path in evidence=%s error=%v", raw, err)
			}
		})
	}
}

func TestRunnerRunningBuildAfterExecutableReplacement(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires replacing the pathname of a running executable")
	}
	const helper = "DETENT_TEST_REPLACED_EXECUTABLE"
	if os.Getenv(helper) == "1" {
		encoder := json.NewEncoder(os.Stdout)
		if err := encoder.Encode("ready"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		info := buildinfo.Info{Version: "1.2.3", Commit: strings.Repeat("a", 40)}
		if err := encoder.Encode(runnerRunningBuild(t.Context(), info, "")); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, action := range []string{"unchanged", "replace path", "change inode metadata"} {
		t.Run(action, func(t *testing.T) {
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(self)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "detent")
			if err := os.WriteFile(path, original, 0o755); err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(dir, "replacement")
			if err := os.WriteFile(replacement, []byte("another release"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), path, "-test.run=^TestRunnerRunningBuildAfterExecutableReplacement$")
			cmd.Env = append(os.Environ(), helper+"=1", "GOCOVERDIR="+t.TempDir())
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			decoder := json.NewDecoder(stdout)
			var ready string
			if err := decoder.Decode(&ready); err != nil || ready != "ready" {
				t.Fatalf("child ready = %q, error = %v", ready, err)
			}
			switch action {
			case "replace path":
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "change inode metadata":
				if err := os.Chmod(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			var evidence runnerauth.BuildEvidence
			if err := decoder.Decode(&evidence); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%x", sha256.Sum256(original))
			if action != "unchanged" && runtime.GOOS == "darwin" {
				want = ""
			}
			if evidence.SHA256 != want || evidence.VerifiedRelease {
				t.Fatalf("running evidence = %+v, want SHA-256 %q", evidence, want)
			}
		})
	}
}
