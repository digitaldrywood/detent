package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/failureattribution"
)

func TestLandingMergeResolutionBoundsConflictReads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, before, after string
		wantCalls           int
		wantError           bool
		validationCommand   string
		wantAttribution     string
		baseMoved           bool
		phaseHistory        bool
	}{
		{name: "symlink conflict uses Git entries", before: "symlink", after: "symlink", wantCalls: 1},
		{name: "phase resolution keeps published ancestry and validates the final commit", before: "symlink", after: "symlink", wantCalls: 1, phaseHistory: true},
		{name: "inspected base advancement stops before provider", baseMoved: true},
		{name: "gate failure reproduces on integration base", before: "symlink", after: "symlink", wantCalls: 1, validationCommand: "printf 'landing validation failed\\n'; exit 1", wantAttribution: failureattribution.Independent},
		{name: "gate failure introduced by resolution", before: "symlink", after: "symlink", wantCalls: 1, validationCommand: "if [ \"$(readlink feature.txt)\" = missing-resolved-target ]; then printf 'landing validation failed\\n'; exit 1; fi", wantAttribution: failureattribution.Change},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newLandingFixture(t)
			outside := filepath.Join(t.TempDir(), "target.txt")
			if err := os.WriteFile(outside, []byte("outside\n<<<<<<< HEAD\nbase\n=======\nreviewed\n>>>>>>> change\nkeep\n"), 0600); err != nil {
				t.Fatal(err)
			}
			write := func(dir, kind string) {
				t.Helper()
				path := filepath.Join(dir, "feature.txt")
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				case "oversized":
					file, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := errors.Join(file.Truncate(16<<20+1), file.Close()); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.WriteFile(path, []byte("base\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			write(fixture.source, test.before)
			runGit(t, fixture.source, "add", "feature.txt")
			runGit(t, fixture.source, "commit", "-m", "base change")
			runGit(t, fixture.source, "push", "origin", "main")
			if test.before == "symlink" {
				if err := os.Remove(filepath.Join(fixture.info.Path, "feature.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing-reviewed-target", filepath.Join(fixture.info.Path, "feature.txt")); err != nil {
					t.Fatal(err)
				}
				runGit(t, fixture.info.Path, "commit", "-am", "reviewed target")
				fixture.head = strings.TrimSpace(runGit(t, fixture.info.Path, "rev-parse", "HEAD"))
			}
			base := fixture.remoteMain(t)
			expectedBase := ""
			if test.baseMoved {
				observed, conflicted, err := fixture.backend.InspectLandingConflict(t.Context(), fixture.info, fixture.issue, LandOptions{HeadSHA: fixture.head, TargetBranch: "main"})
				if err != nil || !conflicted || observed != base || fixture.remoteMain(t) != base || strings.TrimSpace(runGit(t, fixture.info.Path, "rev-parse", "HEAD")) != fixture.head {
					t.Fatalf("read-only inspection lost current conflict/source: base=%s conflicted=%t error=%v", observed, conflicted, err)
				}
				expectedBase = observed
				fixture.advanceMain(t, "ADVANCED.md", "advanced target\n")
				base = fixture.remoteMain(t)
			}
			calls := 0
			command := test.validationCommand
			if command == "" {
				command = "git diff --check HEAD^ HEAD"
			}
			if test.phaseHistory {
				runGit(t, fixture.info.Path, "push", "origin", fixture.head+":refs/heads/"+fixture.info.PublishBranch())
			}
			result, err := fixture.backend.ResolveLandingConflict(t.Context(), fixture.info, fixture.issue, LandOptions{
				HeadSHA: fixture.head, TargetBranch: "main", Message: "resolved change", ValidationCommand: command, ExpectedBaseSHA: expectedBase, PreserveSourceHistory: test.phaseHistory,
			}, func(ctx context.Context, path string, files []string) error {
				calls++
				if len(files) != 1 || files[0] != "feature.txt" {
					t.Fatalf("conflict files = %q", files)
				}
				if test.before == "symlink" {
					if err := os.Remove(filepath.Join(path, "feature.txt")); err != nil {
						return err
					}
					if err := os.Symlink("missing-resolved-target", filepath.Join(path, "feature.txt")); err != nil {
						return err
					}
				} else {
					write(path, test.after)
				}
				_, err := runGitAt(ctx, path, "add", "feature.txt")
				return err
			})
			if test.baseMoved {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA != base || calls != 0 || result.HeadSHA != "" || result.Gate.Command != "" || fixture.remoteMain(t) != base || strings.TrimSpace(runGit(t, fixture.info.Path, "rev-parse", "HEAD")) != fixture.head {
					t.Fatalf("base advancement bypassed inspection fence: result=%+v error=%v calls=%d", result, err, calls)
				}
				path, err := landingRecordPath(t.Context(), fixture.info.Path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexecuted resolution created a landing receipt: %v", err)
				}
				return
			}
			if test.wantAttribution != "" {
				var validationErr *ValidationError
				if !errors.As(err, &validationErr) || validationErr.Stage != ValidationStageLanding {
					t.Fatalf("resolution error = %v, want landing validation failure", err)
				}
				if result.BaseSHA != base || result.HeadSHA == "" || result.Gate.ExitCode != 1 || result.Gate.Attribution != test.wantAttribution {
					t.Fatalf("gate lost failure attribution: %#v", result)
				}
				if reproduction := result.Gate.BaseReproduction; reproduction == nil || !reproduction.Complete || reproduction.HeadSHA != base || reproduction.Command != command {
					t.Fatalf("gate did not reproduce on integration base %s: %#v", base, reproduction)
				}
			} else if errors.Is(err, ErrMergeResolutionInvalid) != test.wantError || err != nil && !test.wantError {
				t.Fatalf("resolution error = %v, want invalid = %v", err, test.wantError)
			}
			if calls != test.wantCalls || len(result.ConflictFiles) != 1 || result.ConflictFiles[0] != "feature.txt" || fixture.remoteMain(t) != base {
				t.Fatalf("resolution lost conflict ownership: calls %d, result %#v", calls, result)
			}
			if test.wantError {
				if !result.Gate.StartedAt.IsZero() || result.HeadSHA != "" {
					t.Fatalf("unsafe source reached commit or gate: %#v", result)
				}
			} else if test.wantAttribution == "" && (result.Unchanged || result.HeadSHA == "" || result.Gate.ExitCode != 0 || result.Gate.StartedAt.IsZero()) {
				t.Fatalf("symlink resolution did not require new review: %#v", result)
			}
			if test.phaseHistory {
				parents := strings.Fields(runGit(t, fixture.info.Path, "show", "-s", "--format=%P", result.HeadSHA))
				if len(parents) != 2 || parents[0] != base || parents[1] != fixture.head || result.Gate.HeadSHA != result.HeadSHA {
					t.Fatalf("resolution parents=%v, gate head=%s, resolution head=%s", parents, result.Gate.HeadSHA, result.HeadSHA)
				}
				fixture.issue.PublicationRepository = fixture.remote
				_, published, _, err := fixture.backend.PushItemPhase(t.Context(), fixture.info, fixture.issue, func(ctx context.Context) error { return ctx.Err() })
				if err != nil || published != result.HeadSHA {
					t.Fatalf("publish resolved item phase: head=%s error=%v", published, err)
				}
				runGit(t, fixture.remote, "merge-base", "--is-ancestor", fixture.head, published)
			}
		})
	}
}

func TestLandingMergeResolutionRejectsDifferentPatch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, resolved string }{
		{name: "same text at another occurrence", resolved: "value\nkeep\nchanged\n"},
		{name: "whitespace differs from reviewed source", resolved: " changed \nkeep\nvalue\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := initSourceRepo(t)
			write := func(content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("value\nkeep\nvalue\n")
			runGit(t, source, "add", "feature.txt")
			runGit(t, source, "commit", "-m", "base")
			base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			write("changed\nkeep\nvalue\n")
			runGit(t, source, "commit", "-am", "reviewed")
			reviewed := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			runGit(t, source, "reset", "--hard", base)
			write(test.resolved)
			runGit(t, source, "commit", "-am", "resolved")
			resolved := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
			same, err := sameLandingDiff(t.Context(), source, reviewed, resolved, base)
			if err != nil {
				t.Fatal(err)
			}
			if same {
				t.Fatal("different source reused the original review")
			}
		})
	}
}
