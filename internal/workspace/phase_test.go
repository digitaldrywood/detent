package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
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

func TestItemPhasePushRefusesDivergentRemote(t *testing.T) {
	f := newLandingFixture(t)
	f.issue.PublicationRepository = f.remote
	ref := "refs/heads/" + f.info.PublishBranch()
	runGit(t, f.source, "push", "origin", "main:"+ref)
	writeFileDiffFile(t, f.source, "remote.txt", "remote work\n")
	runGit(t, f.source, "add", "remote.txt")
	runGit(t, f.source, "commit", "-m", "advance item branch independently")
	runGit(t, f.source, "push", "origin", "main:"+ref)
	remote := strings.TrimSpace(runGit(t, f.remote, "rev-parse", ref))

	_, _, _, err := f.backend.PushItemPhase(t.Context(), f.info, f.issue, func(context.Context) error { return nil })
	var refusal *LandRefusal
	if !errors.As(err, &refusal) || refusal.Kind != LandRefusalHeadMoved {
		t.Fatalf("push refusal = %v, want moved item head", err)
	}
	if head := strings.TrimSpace(runGit(t, f.remote, "rev-parse", ref)); head != remote {
		t.Fatalf("remote head = %s, want %s", head, remote)
	}
}

func TestReplacementItemPhaseCheckout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		receipted bool
		unrelated bool
		override  string
		advance   bool
		want      string
	}{
		{name: "receipted descendant of the current Change", receipted: true},
		{name: "existing ref without a receipt", want: "no matching phase receipt"},
		{name: "receipt from unrelated Change", receipted: true, unrelated: true, want: "does not descend"},
		{name: "replacement object forges ancestry", receipted: true, unrelated: true, override: "replace", want: "does not descend"},
		{name: "graft forges ancestry", receipted: true, unrelated: true, override: "graft", want: "does not descend"},
		{name: "newer Git tip without a receipt", receipted: true, advance: true, want: "differs from its latest phase receipt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLandingFixture(t)
			branch := f.info.PublishBranch()
			ref := "refs/heads/" + branch
			runGit(t, f.info.Path, "push", "origin", "HEAD:"+ref)
			base := f.remoteMain(t)
			receipt := tracker.NativePhaseReceipt{
				Branch: branch, HeadSHA: f.head,
				TreeSHA: strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD^{tree}")),
			}
			if tc.advance {
				runGit(t, f.info.Path, "commit", "--allow-empty", "-m", "unreceipted phase")
				runGit(t, f.info.Path, "push", "origin", "HEAD:"+ref)
			}
			source := initSourceRepo(t)
			runGit(t, source, "remote", "add", "origin", f.remote)
			replacement, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := f.issue
			issue.PublicationRepository = f.remote
			issue.VerifyPhaseBranch = true
			if tc.receipted {
				issue.PhaseSource, issue.PhaseChangeHeadSHA = &receipt, base
			}
			if tc.unrelated {
				writeFileDiffFile(t, source, "unrelated.txt", "unrelated work\n")
				runGit(t, source, "add", "unrelated.txt")
				runGit(t, source, "commit", "-m", "publish different Change source")
				issue.PhaseChangeHeadSHA = strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			}
			if tc.override != "" {
				runGit(t, source, "fetch", "origin", ref)
				switch tc.override {
				case "replace":
					forged := strings.TrimSpace(runGit(t, source, "commit-tree", receipt.TreeSHA, "-p", issue.PhaseChangeHeadSHA, "-m", "forge parent"))
					runGit(t, source, "replace", receipt.HeadSHA, forged)
				case "graft":
					runGit(t, source, "config", "advice.graftFileDeprecated", "false")
					grafts := filepath.Join(source, ".git", "info", "grafts")
					if err := os.WriteFile(grafts, []byte(receipt.HeadSHA+" "+issue.PhaseChangeHeadSHA+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				runGit(t, source, "merge-base", "--is-ancestor", issue.PhaseChangeHeadSHA, receipt.HeadSHA)
			}
			issue.FreshCheckout, issue.BaseRef = true, base
			info, err := replacement.Create(t.Context(), issue)
			if tc.want != "" {
				if !errors.Is(err, ErrItemPhaseSource) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("checkout refusal = %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if head, err := replacement.Head(t.Context(), info, issue); err != nil || head != receipt.HeadSHA {
				t.Fatalf("replacement head = %s, error = %v, want %s", head, err, receipt.HeadSHA)
			}
			runGit(t, info.Path, "commit", "--allow-empty", "-m", "complete next phase")
			_, head, _, err := replacement.PushItemPhase(t.Context(), info, issue, func(context.Context) error { return nil })
			if remote := strings.TrimSpace(runGit(t, f.remote, "rev-parse", ref)); err != nil || remote != head {
				t.Fatalf("push head = %s, remote = %s, error = %v", head, remote, err)
			}
		})
	}
}

func TestItemPhasePushReconcilesConcurrentRefUpdate(t *testing.T) {
	for _, sameHead := range []bool{true, false} {
		name := "divergent head"
		if sameHead {
			name = "intended head"
		}
		t.Run(name, func(t *testing.T) {
			f := newLandingFixture(t)
			f.issue.PublicationRepository = f.remote
			ref := "refs/heads/" + f.info.PublishBranch()
			intended := f.head
			concurrent := intended
			if !sameHead {
				concurrent = strings.TrimSpace(runGit(t, f.info.Path, "rev-parse", "HEAD^"))
			}
			calls := 0
			_, head, _, err := f.backend.PushItemPhase(t.Context(), f.info, f.issue, func(context.Context) error {
				calls++
				if calls == 2 {
					runGit(t, f.info.Path, "push", "origin", concurrent+":"+ref)
				}
				return nil
			})
			var refusal *LandRefusal
			if sameHead && (err != nil || head != intended) {
				t.Fatalf("publication = %s, error = %v, want %s", head, err, intended)
			}
			if !sameHead && (!errors.As(err, &refusal) || refusal.Kind != LandRefusalHeadMoved) {
				t.Fatalf("publication error = %v, want moved item branch", err)
			}
			if remote := strings.TrimSpace(runGit(t, f.remote, "rev-parse", ref)); remote != concurrent {
				t.Fatalf("remote head = %s, want %s", remote, concurrent)
			}
		})
	}
}

func TestAbsentItemPhaseBranchDoesNotAuthorizeConcurrentTip(t *testing.T) {
	f := newLandingFixture(t)
	base := f.remoteMain(t)
	runGit(t, f.info.Path, "push", "origin", "HEAD:refs/heads/seed")
	source := initSourceRepo(t)
	runGit(t, source, "remote", "add", "origin", f.remote)
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ref-created")
	ref := "refs/heads/" + f.info.PublishBranch()
	git := shellQuote(realGit)
	script := "#!/bin/sh\ncase \"$*\" in *ls-remote*" + shellQuote(ref) + ")\n" +
		"if [ ! -f " + shellQuote(marker) + " ]; then\n" +
		git + " \"$@\" || exit $?\n" +
		git + " -C " + shellQuote(f.remote) + " update-ref " + shellQuote(ref) + " " + shellQuote(f.head) + " || exit $?\n" +
		"touch " + shellQuote(marker) + " && exit 0\nfi;;\nesac\nexec " + git + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	issue := f.issue
	issue.PublicationRepository, issue.VerifyPhaseBranch, issue.FreshCheckout = f.remote, true, true
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("did not create the concurrent ref: %v", err)
	}
	if head, err := backend.Head(t.Context(), info, issue); err != nil || head != base {
		t.Fatalf("checkout head = %s, error = %v, want independently selected base %s", head, err, base)
	}
}
