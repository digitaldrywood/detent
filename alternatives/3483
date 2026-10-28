package workspace

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCaptureChangeSourceCompressesRevisions(t *testing.T) {
	for _, packed := range []bool{false, true} {
		name := "loose"
		if packed {
			name = "packed"
		}
		t.Run(name, func(t *testing.T) {
			source := initSourceRepo(t)
			runGit(t, source, "config", "gc.auto", "0")
			checkpointWrite(t, source, ".gitattributes", "*.bin -diff\n")
			checkpointWrite(t, source, "nested/.gitattributes", "*.bin -text\n")
			runGit(t, source, "add", ".gitattributes", "nested/.gitattributes")
			runGit(t, source, "commit", "-m", "inherited attributes")
			data := make([]byte, 5<<20)
			if _, err := rand.Read(data); err != nil {
				t.Fatal(err)
			}
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			for i := range 5 {
				data[i] ^= 1
				if err := os.WriteFile(filepath.Join(source, "nested/blob.bin"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "add", "nested/blob.bin")
				if i == 4 {
					runGit(t, source, "rm", ".gitattributes")
				}
				runGit(t, source, "commit", "-m", "blob revision")
			}
			if packed {
				runGit(t, source, "repack", "-ad")
			}
			head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			capture, err := CaptureChangeSource(t.Context(), source, base, head)
			if err != nil {
				t.Fatal(err)
			}
			if len(capture.Bundle) >= 2*len(data) {
				t.Fatalf("repeated blob revisions were not compressed: bundle bytes=%d", len(capture.Bundle))
			}
			if err := VerifyChangeSource(t.Context(), capture.Source, capture.Bundle); err != nil {
				t.Fatalf("captured pack cannot be verified without repository objects: %v", err)
			}
		})
	}
}

func TestLocalGitChangeSourceRefusesUnverifiedRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	repository := "https://github.com/example/source"
	runGit(t, source, "remote", "add", "origin", repository)
	runGit(t, source, "config", "url."+remote+".insteadOf", repository)
	runGit(t, source, "push", "-u", "origin", "main")
	base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("reviewed source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "source.txt")
	runGit(t, source, "commit", "-m", "unpublished source")
	head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	capture, err := CaptureChangeSource(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	editBundle := func(bundle []byte) func(*ChangeSource) {
		return func(source *ChangeSource) {
			source.Bundle = bundle
			source.Version.Source.BundleSHA256 = tracker.ChangeSourceDigest(bundle)
			source.Version.Source.Bytes = int64(len(bundle))
		}
	}
	for _, test := range []struct {
		name        string
		edit        func(*ChangeSource)
		pending     string
		published   bool
		unpublished bool
		previous    bool
		absent      bool
		corrupt     bool
		transferred bool
		locked      bool
		integrated  bool
	}{
		{name: "additional advertised ref", edit: editBundle(bytes.Replace(capture.Bundle, []byte("\n\n"), []byte("\n"+head+" refs/heads/extra\n\n"), 1))},
		{name: "wrong advertised head", edit: editBundle(bytes.Replace(capture.Bundle, []byte(head+" HEAD"), []byte(base+" HEAD"), 1))},
		{name: "corrupt bundle", edit: func(s *ChangeSource) { s.Bundle = []byte("changed") }},
		{name: "wrong diff", edit: func(s *ChangeSource) { s.Version.Source.DiffSHA256 = strings.Repeat("a", 64) }},
		{name: "wrong repository", edit: func(s *ChangeSource) { s.Version.Repository = "https://github.com/other/source" }},
		{name: "missing legacy source", edit: func(s *ChangeSource) { s.Version.Source = nil; s.Bundle = nil }},
		{name: "staged divergence survives restoration", pending: "index"},
		{name: "mixed staged working and untracked source survives restoration", pending: "mixed"},
		{name: "staged addition with missing working file survives restoration", pending: "addition"},
		{name: "staged deletion with restored working file survives restoration", pending: "deletion"},
		{name: "working source survives restoration", pending: "working"},
		{name: "untracked source survives restoration", pending: "untracked"},
		{name: "newer clean published head survives older source restoration", published: true},
		{name: "integrated target head is preserved and restored", published: true, integrated: true},
		{name: "superseded published source is preserved and restored", published: true, previous: true},
		{name: "corrupt retained source preserves existing workspace", published: true, previous: true, corrupt: true},
		{name: "superseded branch without workspace is preserved and restored", published: true, previous: true, absent: true},
		{name: "superseded Hub-only source is preserved and restored", unpublished: true, previous: true},
		{name: "superseded Hub-only branch without workspace is preserved and restored", unpublished: true, previous: true, absent: true},
		{name: "unknown unpublished source survives restoration", unpublished: true},
		{name: "transferred version refuses locked dirty custody", pending: "mixed", transferred: true, locked: true},
		{name: "transferred version preserves mixed dirty custody", pending: "mixed", transferred: true},
		{name: "transferred version preserves staged-only custody", pending: "index", transferred: true},
		{name: "transferred version preserves untracked custody", pending: "untracked", transferred: true},
		{name: "transferred version preserves unknown unpublished head", unpublished: true, transferred: true},
		{name: "transferred version restores superseded head with custody", published: true, previous: true, transferred: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "second")
			runGit(t, source, "clone", "--no-local", remote, dir)
			runGit(t, dir, "remote", "set-url", "origin", repository)
			backend, err := NewBackend(KindLocalGit, LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: dir, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			metadata := capture.Source
			retained := &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head, Repository: repository, Source: &metadata}}, Bundle: capture.Bundle}
			retained.Version.ID, retained.Transferred = "version_selected", test.transferred
			if test.edit != nil {
				test.edit(retained)
			}
			issue := Issue{Identifier: "native#584", NativeRework: true, Source: retained, BaseRef: base}
			if test.published || test.unpublished {
				runGit(t, dir, "config", "url."+remote+".insteadOf", repository)
				initial := issue
				initial.Source = nil
				info, err := backend.Create(t.Context(), initial)
				if err != nil {
					t.Fatal(err)
				}
				runGit(t, source, "push", "origin", head+":refs/heads/retained-work")
				runGit(t, dir, "fetch", "origin")
				start, publishedRef := head, "refs/heads/published-work-"+SafeKey(test.name)
				if test.integrated {
					start, publishedRef = base, "refs/heads/integration-"+SafeKey(test.name)
					issue.ProgressBaseRef = strings.TrimPrefix(publishedRef, "refs/heads/")
					initial.ProgressBaseRef = issue.ProgressBaseRef
				}
				runGit(t, info.Path, "reset", "--hard", start)
				writeFileDiffFile(t, info.Path, "source.txt", "newer completed work\n")
				runGit(t, info.Path, "add", "source.txt")
				runGit(t, info.Path, "-c", "user.name=Recovery Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", test.name)
				newer := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
				if test.published {
					runGit(t, info.Path, "push", "origin", "HEAD:"+publishedRef)
					runGit(t, dir, "fetch", "origin")
				}
				before, err := backend.(RecoveryStateProvider).RecoveryState(t.Context(), info, initial)
				if err != nil || before.HeadSHA != newer || (before.UnpushedCommits > 0) != test.unpublished || len(before.TrackedPaths) != 0 || len(before.UntrackedPaths) != 0 {
					t.Fatalf("fixture lacks expected clean commit publication state: %+v, %v", before, err)
				}
				if test.previous {
					retained.PreviousHeads = []string{newer}
				}
				if test.corrupt {
					retained.Bundle = []byte("corrupt bundle")
				}
				if test.absent {
					runGit(t, dir, "worktree", "remove", info.Path)
				}
				restored, restoreErr := backend.Create(t.Context(), issue)
				if (test.previous || test.transferred || test.integrated) && !test.corrupt {
					if restoreErr != nil {
						t.Fatalf("restore superseded source: %v", restoreErr)
					}
					if got := strings.TrimSpace(runGit(t, restored.Path, "rev-parse", "HEAD")); got != head || !restored.Created {
						t.Fatalf("restored source head=%s created=%v", got, restored.Created)
					}
					if got := readFile(t, filepath.Join(restored.Path, "source.txt")); got != "reviewed source\n" {
						t.Fatalf("restored source bytes=%q", got)
					}
					paths, err := filepath.Glob(filepath.Join(backend.(*LocalGit).root, ".detent", "quarantine", "*"))
					if err != nil || len(paths) != 1 {
						t.Fatalf("preserved workspaces=%v: %v", paths, err)
					}
					if got := strings.TrimSpace(runGit(t, paths[0], "rev-parse", "HEAD")); got != newer {
						t.Fatalf("preserved head=%s want=%s", got, newer)
					}
					if got := readFile(t, filepath.Join(paths[0], "source.txt")); got != "newer completed work\n" {
						t.Fatalf("preserved source bytes=%q", got)
					}
					return
				}
				var refusal *LandRefusal
				if test.unpublished && !test.corrupt && !test.transferred {
					if restoreErr != nil || restored.Created {
						t.Fatalf("unpublished work must remain available unchanged: %+v, %v", restored, restoreErr)
					}
				} else if test.corrupt && restoreErr == nil || !test.corrupt && (!errors.As(restoreErr, &refusal) || refusal.Kind != LandRefusalHeadMoved) {
					t.Errorf("older retained source must require explicit reconciliation: %v", restoreErr)
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != newer {
					t.Errorf("source restoration replaced newer published head: got=%s want=%s", got, newer)
				}
				if got := readFile(t, filepath.Join(info.Path, "source.txt")); got != "newer completed work\n" {
					t.Errorf("source restoration replaced newer published bytes: %q", got)
				}
				return
			}
			if test.pending != "" {
				runGit(t, dir, "config", "url."+remote+".insteadOf", repository)
				initial := issue
				initial.Source = nil
				info, err := backend.Create(t.Context(), initial)
				if err != nil {
					t.Fatal(err)
				}
				provider := backend.(RecoveryStateProvider)
				clean, err := provider.RecoveryState(t.Context(), info, initial)
				if err != nil {
					t.Fatal(err)
				}
				if clean.HeadSHA != base || clean.UnpushedCommits != 0 || len(clean.TrackedPaths) != 0 || len(clean.UntrackedPaths) != 0 {
					t.Fatalf("initial source is not the clean published base: %+v", clean)
				}
				wantFiles := map[string]string{"README.md": "source repo\n"}
				wantTracked := []string{"README.md"}
				var wantUntracked []string
				switch test.pending {
				case "index", "mixed":
					writeFileDiffFile(t, info.Path, "README.md", "staged source B\n")
					runGit(t, info.Path, "add", "README.md")
					if test.pending == "mixed" {
						wantFiles["README.md"] = "working source C\n"
						wantFiles["pending.go"] = "untracked sentinel\n"
						wantUntracked = []string{"pending.go"}
						writeFileDiffFile(t, info.Path, "pending.go", wantFiles["pending.go"])
					}
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					if got := runGit(t, info.Path, "show", ":README.md"); got != "staged source B\n" {
						t.Fatalf("fixture lacks staged divergence: %q", got)
					}
				case "addition":
					writeFileDiffFile(t, info.Path, "added.go", "staged addition\n")
					runGit(t, info.Path, "add", "added.go")
					if err := os.Remove(filepath.Join(info.Path, "added.go")); err != nil {
						t.Fatal(err)
					}
					wantTracked = []string{"added.go"}
				case "deletion":
					runGit(t, info.Path, "rm", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					wantUntracked = []string{"README.md"}
				case "working":
					wantFiles["README.md"] = "working source C\n"
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
				case "untracked":
					wantTracked = nil
					wantUntracked = []string{"pending.go"}
					wantFiles["pending.go"] = "untracked sentinel\n"
					writeFileDiffFile(t, info.Path, "pending.go", wantFiles["pending.go"])
				}
				if test.pending == "index" || test.pending == "addition" {
					if got := runGit(t, info.Path, "diff", "HEAD", "--"); got != "" {
						t.Fatalf("fixture working bytes differ from HEAD: %q", got)
					}
				}
				staged := runGit(t, info.Path, "ls-files", "--stage", "-z")
				observed, err := provider.RecoveryState(t.Context(), info, initial)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(observed.TrackedPaths, wantTracked) || !slices.Equal(observed.UntrackedPaths, wantUntracked) || observed.DiffStat.IsEmpty() || observed.WorkspaceFingerprint == clean.WorkspaceFingerprint {
					t.Errorf("pending source was not observed: %+v; want tracked=%v untracked=%v", observed, wantTracked, wantUntracked)
				}
				if got := runGit(t, info.Path, "ls-files", "--stage", "-z"); got != staged {
					t.Errorf("observation changed staged entries: got=%q want=%q", got, staged)
				}
				if test.pending == "index" {
					writeFileDiffFile(t, info.Path, "README.md", "alternate staged source\n")
					runGit(t, info.Path, "add", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
					changed, err := provider.RecoveryState(t.Context(), info, initial)
					if err != nil || changed.WorkspaceFingerprint == observed.WorkspaceFingerprint {
						t.Errorf("staged content changed without changing recovery identity: %+v, %v", changed, err)
					}
					writeFileDiffFile(t, info.Path, "README.md", "staged source B\n")
					runGit(t, info.Path, "add", "README.md")
					writeFileDiffFile(t, info.Path, "README.md", wantFiles["README.md"])
				}
				if test.locked {
					runGit(t, dir, "worktree", "lock", info.Path)
				}
				restored, restoreErr := backend.Create(t.Context(), issue)
				if test.locked {
					var refusal *LandRefusal
					if !errors.As(restoreErr, &refusal) || refusal.Kind != LandRefusalHeadMoved || !strings.Contains(refusal.Reason, retained.Version.ID) || !strings.Contains(refusal.Reason, "source recovery") {
						t.Fatalf("locked custody lacks an actionable refusal: %v", restoreErr)
					}
				} else if test.transferred {
					if restoreErr != nil || !restored.Created || strings.TrimSpace(runGit(t, restored.Path, "rev-parse", "HEAD")) != head || runGit(t, restored.Path, "status", "--porcelain") != "" {
						t.Fatalf("transfer did not restore clean selected source: %+v, %v", restored, restoreErr)
					}
					paths, err := filepath.Glob(filepath.Join(backend.(*LocalGit).root, ".detent", "quarantine", "*"))
					if err != nil || len(paths) != 1 {
						t.Fatalf("preserved workspaces=%v: %v", paths, err)
					}
					info.Path = paths[0]
				}
				if restoreErr != nil {
					t.Logf("source restoration refused: %v", restoreErr)
				}
				if got := runGit(t, info.Path, "ls-files", "--stage", "-z"); got != staged {
					t.Errorf("source restoration discarded staged entries: got=%q want=%q", got, staged)
				}
				for path, want := range wantFiles {
					if got := readFile(t, filepath.Join(info.Path, path)); got != want {
						t.Errorf("source restoration changed %s working bytes: got=%q want=%q", path, got, want)
					}
				}
				if test.pending == "addition" {
					if _, err := os.Stat(filepath.Join(info.Path, "added.go")); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("source restoration changed missing working file: %v", err)
					}
				}
				if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != base {
					t.Errorf("source restoration moved pending-source head: got=%s want=%s", got, base)
				}
				return
			}
			if _, err := backend.Create(t.Context(), issue); err == nil {
				t.Fatal("unverified source started a worker checkout")
			} else if test.name == "missing legacy source" && !strings.Contains(err.Error(), "source-owning runner") {
				t.Fatalf("legacy recovery lacks a concrete supported action: %v", err)
			}
			if _, err := backend.(*LocalGit).Existing(issue); !errors.Is(err, ErrMissingWorkspace) {
				t.Fatalf("refused recovery issued a fresh base checkout: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name        string
		path        string
		content     string
		allowed     bool
		credential  bool
		emptyCommit bool
	}{
		{name: "host setup", path: ".detent/environment.md"},
		{name: "env", path: ".env"},
		{name: "local env", path: ".env.local"},
		{name: "production env", path: "config/.env.production"},
		{name: "envrc", path: ".envrc"},
		{name: "private ssh key", path: "keys/id_rsa"},
		{name: "public ssh key", path: "keys/id_rsa.pub"},
		{name: "pem", path: "keys/certificate.pem"},
		{name: "key", path: "keys/certificate.key"},
		{name: "p12", path: "keys/certificate.p12"},
		{name: "env template", path: ".env.example", content: "API_TOKEN=''\n", allowed: true},
		{name: "envrc template", path: ".envrc.example", content: "export API_TOKEN=''\n", allowed: true},
		{name: "source with empty commit", path: "history.txt", content: "reviewed source\n", allowed: true, emptyCommit: true},
		{name: "nested env template", path: "config/.env.production.example", content: "API_TOKEN=''\n", allowed: true},
		{name: "env template credential", path: ".env.example", content: "API_TOKEN=ghp_" + strings.Repeat("Z", 36), credential: true},
		{name: "envrc template credential", path: ".envrc.example", content: "export API_TOKEN=ghp_" + strings.Repeat("Z", 36), credential: true},
	} {
		t.Run("capture path "+test.name, func(t *testing.T) {
			runGit(t, source, "reset", "--hard", head)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(source, test.path)), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, test.path), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "--force", test.path)
			runGit(t, source, "commit", "-m", "source path fixture")
			if test.emptyCommit {
				runGit(t, source, "commit", "--allow-empty", "-m", "empty source history")
			}
			selectedHead := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			capture, err := CaptureChangeSource(t.Context(), source, base, selectedHead)
			if test.allowed {
				if err != nil {
					t.Fatal(err)
				}
				if err := capture.Source.Validate(base, selectedHead, capture.Bundle); err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrCheckpointUnsafe) || len(capture.Bundle) != 0 {
				t.Fatalf("source capture uploaded unsafe material: bytes=%d, %v", len(capture.Bundle), err)
			}
			if test.credential && err.Error() != "checkpoint could not be safely published: possible credential in selected content" {
				t.Fatalf("template credential was not refused by the content scan: %v", err)
			}
		})
	}

	for _, scenario := range []string{"inherited synthetic fixtures", "runtime synthetic checkpoint fixtures", "introduced credential", "inherited credential", "inherited attributes credential", "unsafe intermediate commit"} {
		t.Run("capture "+scenario, func(t *testing.T) {
			source := initSourceRepo(t)
			paths := []string{"internal/hubserver/health_email_test.go", "internal/hubserver/slack_delivery_test.go"}
			for _, path := range paths {
				data, err := os.ReadFile(filepath.Join("..", "hubserver", filepath.Base(path)))
				if err != nil {
					t.Fatal(err)
				}
				checkpointWrite(t, source, path, string(data))
			}
			if scenario == "runtime synthetic checkpoint fixtures" {
				path := "internal/workspace/checkpoint_ref_test.go"
				data, err := os.ReadFile("checkpoint_ref_test.go")
				if err != nil {
					t.Fatal(err)
				}
				checkpointWrite(t, source, path, string(data))
				paths = append(paths, path)
			}
			credential := "ghp_" + strings.Repeat("Z", 36)
			if scenario == "inherited attributes credential" {
				checkpointWrite(t, source, ".gitattributes", "* token="+credential+"\n")
				runGit(t, source, "add", ".gitattributes")
			}
			if scenario == "inherited credential" {
				checkpointWrite(t, source, paths[0], readFile(t, filepath.Join(source, paths[0]))+"\n"+credential)
			}
			runGit(t, source, append([]string{"add", "--"}, paths...)...)
			runGit(t, source, "commit", "-m", "inherited notification fixtures")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			if scenario == "unsafe intermediate commit" {
				original := readFile(t, filepath.Join(source, paths[0]))
				checkpointWrite(t, source, paths[0], original+"\n"+credential)
				runGit(t, source, "add", "--", paths[0])
				runGit(t, source, "commit", "-m", "unsafe intermediate fixture")
				checkpointWrite(t, source, paths[0], original)
			}
			for _, path := range paths {
				checkpointWrite(t, source, path, readFile(t, filepath.Join(source, path))+"\n")
			}
			if scenario == "introduced credential" {
				checkpointWrite(t, source, paths[1], readFile(t, filepath.Join(source, paths[1]))+credential)
			}
			runGit(t, source, append([]string{"add", "--"}, paths...)...)
			runGit(t, source, "commit", "-m", "selected notification source")
			head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			tree := runGit(t, source, "rev-parse", "HEAD^{tree}")
			capture, err := CaptureChangeSource(t.Context(), source, base, head)
			if scenario != "inherited synthetic fixtures" && scenario != "runtime synthetic checkpoint fixtures" {
				if !errors.Is(err, ErrCheckpointUnsafe) || err.Error() != "checkpoint could not be safely published: possible credential in selected content" || len(capture.Bundle) != 0 {
					t.Fatalf("unsafe source escaped capture refusal: bytes=%d, error=%v", len(capture.Bundle), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := capture.Source.Validate(base, head, capture.Bundle); err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(runGit(t, source, "diff", "--name-only", base, head)); !slices.Equal(got, paths) || runGit(t, source, "rev-parse", "HEAD^{tree}") != tree {
				t.Fatal("capture changed selected paths or exact source tree")
			}
		})
	}

}

// A release pick may exist only in the retained bundle on a fresh runner.
func TestLocalGitReleaseDeliveryImportsRetainedHeadBeforeReview(t *testing.T) {
	source := initSourceRepo(t)
	remote := initBareRemote(t)
	repository := "https://github.com/example/source"
	runGit(t, source, "remote", "add", "origin", repository)
	runGit(t, source, "config", "url."+remote+".insteadOf", repository)
	runGit(t, source, "push", "origin", "HEAD:refs/heads/release/v1.2", "HEAD:refs/heads/main")
	base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	checkpointWrite(t, source, "repair.txt", "reviewed repair\n")
	runGit(t, source, "add", "repair.txt")
	runGit(t, source, "commit", "-m", "release repair")
	head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	capture, err := CaptureChangeSource(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	receiver := filepath.Join(t.TempDir(), "receiver")
	runGit(t, source, "clone", "--no-local", "--branch", "release/v1.2", remote, receiver)
	runGit(t, receiver, "remote", "set-url", "origin", repository)
	runGit(t, receiver, "config", "url."+remote+".insteadOf", repository+".git")
	runGit(t, receiver, "config", "--add", "url."+remote+".insteadOf", repository)
	if _, err := runGitAt(t.Context(), receiver, "cat-file", "-e", head+"^{commit}"); err == nil {
		t.Fatal("fixture already has retained head")
	}
	backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: receiver, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "native#42", Source: &ChangeSource{Version: tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: base, HeadSHA: head, Repository: repository, Source: &capture.Source}}, Bundle: capture.Bundle}, ReleaseDelivery: &tracker.ReleaseDelivery{ReleasePickInput: tracker.ReleasePickInput{Repository: repository, PRNumber: 42, BaseRef: "release/v1.2", HeadRef: "release-pick/repair", BaseSHA: base, HeadSHA: head}}}
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
		t.Fatalf("head = %s, want %s", got, head)
	}
	if got := readFile(t, filepath.Join(info.Path, "repair.txt")); got != "reviewed repair\n" {
		t.Fatalf("repair = %q", got)
	}
	if got := strings.TrimSpace(runGit(t, remote, "rev-parse", "refs/heads/release/v1.2")); got != base {
		t.Fatalf("release branch moved to %s", got)
	}
}
