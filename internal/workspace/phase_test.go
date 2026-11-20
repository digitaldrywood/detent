package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func verifyPhasePublication(t *testing.T) {
	t.Helper()
	for _, test := range []struct {
		name, wantError, configKey string
		configure                  func(*testing.T, landingFixture)
		deny                       bool
		noDestination              bool
	}{
		{name: "clean"},
		{name: "split index", configure: func(t *testing.T, f landingFixture) {
			runGit(t, f.info.Path, "update-index", "--split-index")
		}},
		{name: "configured filesystem monitor", configKey: "core.fsmonitor"},
		{name: "configured required filter", configKey: "filter.example.clean", configure: func(t *testing.T, f landingFixture) {
			writeFileDiffFile(t, f.info.Path, ".gitattributes", "feature.txt filter=example\n")
			runGit(t, f.info.Path, "add", ".gitattributes")
			runGit(t, f.info.Path, "commit", "-m", "record attributes")
			runGit(t, f.info.Path, "config", "filter.example.required", "true")
			stamp := time.Unix(1700000000, 0)
			if err := os.Chtimes(filepath.Join(f.info.Path, "feature.txt"), stamp, stamp); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "local changes", wantError: "review workspace has local changes", configure: func(t *testing.T, f landingFixture) {
			writeFileDiffFile(t, f.info.Path, "feature.txt", "dirty\n")
		}},
		{name: "external alternate", wantError: "publication object store uses alternates", configure: addPhaseFixtureAlternate},
		{name: "authority comes first", wantError: "phase lease denied", configure: addPhaseFixtureAlternate, deny: true, noDestination: true},
		{name: "destination comes before source", wantError: "publication fetch remote differs", configure: addPhaseFixtureAlternate, noDestination: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newLandingFixture(t)
			f.issue.PublicationRepository = f.remote
			if test.noDestination {
				f.issue.PublicationRepository = ""
			}
			if test.configure != nil {
				test.configure(t, f)
			}
			var trace, probe string
			if test.configKey != "" {
				dir := t.TempDir()
				probe = filepath.Join(dir, "config-probe")
				if err := os.WriteFile(probe, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, f.info.Path, "config", test.configKey, probe)
				trace = filepath.Join(dir, "process.jsonl")
				t.Setenv("GIT_TRACE2_EVENT", trace)
			}
			branch, head, tree, err := f.backend.PushItemPhase(t.Context(), f.info, f.issue, func(context.Context) error {
				if test.deny {
					return errors.New("phase lease denied")
				}
				return nil
			})
			if trace != "" {
				raw, err := os.ReadFile(trace)
				if err != nil || len(raw) == 0 {
					t.Fatalf("read Git process trace: %v", err)
				}
				for event := range strings.SplitSeq(string(raw), "\n") {
					if strings.Contains(event, `"event":"child_start"`) && strings.Contains(event, probe) {
						t.Fatal("phase publication attempted to run a checkout-configured command")
					}
				}
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("push error = %v, want %q", err, test.wantError)
				}
				_, exists, err := gitexec.PublicationBranchHead(t.Context(), f.remote, f.info.PublishBranch())
				if err != nil || exists {
					t.Fatalf("ref must stay absent, exists = %t, error = %v", exists, err)
				}
				return
			}
			wantHead := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD"))
			wantTree := strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD^{tree}"))
			remoteHead, exists, readErr := gitexec.PublicationBranchHead(t.Context(), f.remote, f.info.PublishBranch())
			if err != nil || readErr != nil || !exists || branch != f.info.PublishBranch() || head != wantHead || tree != wantTree || remoteHead != wantHead {
				t.Fatalf("push = %q %q %q, error = %v, remote = %q, error = %v", branch, head, tree, err, remoteHead, readErr)
			}
		})
	}
}

func addPhaseFixtureAlternate(t *testing.T, f landingFixture) {
	t.Helper()
	writeFileDiffFile(t, f.source, ".git/objects/info/alternates", filepath.Join(initBareRemote(t), "objects")+"\n")
}
