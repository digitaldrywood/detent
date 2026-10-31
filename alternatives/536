package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/buildinfo"
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
			if evidence.Validate() != nil || evidence.Version != test.info.Version || evidence.Commit != test.info.Commit || evidence.OS != runtime.GOOS || evidence.Architecture != runtime.GOARCH || len(evidence.SHA256) != 64 || evidence.VerifiedRelease {
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
