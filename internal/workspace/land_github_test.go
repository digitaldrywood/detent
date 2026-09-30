package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGitLandChangeViaGitHub(t *testing.T) {
	for _, test := range []struct {
		name       string
		mergeError bool
		method     string
		pullState  string
		reworked   bool
	}{
		{name: "merges the reviewed head", method: "merge"},
		{name: "uses the policy squash method", method: "squash"},
		{name: "uses the policy rebase method", method: "rebase"},
		{name: "reports protected merge refusal", method: "merge", mergeError: true},
		{name: "reuses an open PR", method: "merge", pullState: "open"},
		{name: "records an already merged reviewed head", method: "merge", pullState: "merged"},
		{name: "ignores an older merged PR", method: "merge", pullState: "older"},
		{name: "publishes a reworked branch", method: "squash", reworked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLandingFixture(t)
			base := fixture.remoteMain(t)
			repository := "https://github.com/example/repo"
			// Keep the reviewed URL GitHub-shaped while git sends to the local
			// bare repository. Neither test touches a real GitHub repository.
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			if test.reworked {
				tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
				previous := strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", base, "-m", "Previous attempt"))
				runGit(t, fixture.source, "push", "origin", previous+":refs/heads/"+fixture.info.Branch)
			}
			bin := t.TempDir()
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$TEST_GH_CALLS"
case "$*" in
  *'--method GET'*)
    case "$TEST_PULL_STATE" in
      open) printf '[{"number":7,"state":"open","head":{"sha":"%s"},"base":{"ref":"main"}}]\n' "$TEST_REVIEWED_HEAD" ;;
      merged)
        git --git-dir "$TEST_BARE_REPOSITORY" update-ref refs/heads/main "$TEST_REVIEWED_HEAD" || exit
        printf '[{"number":7,"state":"closed","merged_at":"2026-09-29T00:00:00Z","merge_commit_sha":"%s","head":{"sha":"%s"},"base":{"ref":"main"}}]\n' "$TEST_REVIEWED_HEAD" "$TEST_REVIEWED_HEAD" ;;
      older) printf '[{"number":6,"state":"closed","merged_at":"2026-09-28T00:00:00Z","merge_commit_sha":"%s","head":{"sha":"%s"},"base":{"ref":"main"}}]\n' "$TEST_OLD_HEAD" "$TEST_OLD_HEAD" ;;
      *) printf '[]\n' ;;
    esac ;;
  *'--method POST'*) printf '{"number":7,"state":"open","head":{"sha":"%s"},"base":{"ref":"main"}}\n' "$TEST_REVIEWED_HEAD" ;;
  *'--method PUT'*)
    if [ "$TEST_MERGE_REFUSED" = 1 ]; then
      printf 'HTTP 405: Branch protection requires reviews\n' >&2
      exit 1
    fi
    git --git-dir "$TEST_BARE_REPOSITORY" update-ref refs/heads/main "$TEST_REVIEWED_HEAD" || exit
    printf '{"merged":true,"sha":"%s"}\n' "$TEST_REVIEWED_HEAD" ;;
  *) printf 'unexpected gh command: %s\n' "$*" >&2; exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_BARE_REPOSITORY", fixture.remote)
			t.Setenv("TEST_REVIEWED_HEAD", fixture.head)
			t.Setenv("TEST_OLD_HEAD", base)
			t.Setenv("TEST_PULL_STATE", test.pullState)
			callsPath := filepath.Join(bin, "calls")
			t.Setenv("TEST_GH_CALLS", callsPath)
			if test.mergeError {
				t.Setenv("TEST_MERGE_REFUSED", "1")
			}
			result, err := fixture.backend.LandChangeViaGitHub(context.Background(), fixture.info, fixture.issue, LandOptions{
				HeadSHA: fixture.head, Method: test.method, Repository: repository,
				Message: "Review this change\n\nNative Change Request", PushAttemptBranch: true,
			})
			if test.mergeError {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || fixture.remoteMain(t) == fixture.head {
					t.Fatalf("protected merge = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.MergeSHA != fixture.head || result.BaseRef != "main" || result.Method != test.method || !result.AttemptBranchPushed || fixture.remoteMain(t) != fixture.head {
				t.Fatalf("GitHub landing = %#v; base = %s", result, fixture.remoteMain(t))
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantCreate := test.pullState != "open" && test.pullState != "merged"
			wantMerge := test.pullState != "merged"
			if strings.Contains(string(calls), "--method POST") != wantCreate || strings.Contains(string(calls), "--method PUT") != wantMerge {
				t.Fatalf("unexpected PR operations: %s", calls)
			}
			if wantMerge && (!strings.Contains(string(calls), "merge_method="+test.method) || !strings.Contains(string(calls), "sha="+fixture.head)) {
				t.Fatalf("merge did not name the approved method and head: %s", calls)
			}
			published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
			if published != fixture.head {
				t.Fatalf("published attempt = %s, want %s", published, fixture.head)
			}
			if remoteURL := RepositoryURL(context.Background(), fixture.info.Path); remoteURL != repository {
				t.Fatalf("reviewed repository = %s, want %s", remoteURL, repository)
			}
		})
	}
}

func TestGitHubLandingRepository(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		url, name, owner string
	}{
		{"https://github.com/example/repo", "example/repo", "example"},
		{"https://example.com/example/repo", "", ""},
		{"https://github.com/example/repo/extra", "", ""},
		{"https://github.com/%2e%2e/repo", "", ""},
		{"https://github.com/example/repo?token=secret", "", ""},
		{"https://user:secret@github.com/example/repo", "", ""},
	} {
		t.Run(test.url, func(t *testing.T) {
			name, owner, ok := githubLandingRepository(test.url)
			if name != test.name || owner != test.owner || ok != (test.name != "") {
				t.Fatalf("repository = %q, %q, %v", name, owner, ok)
			}
		})
	}
}

func TestGitHubLandingAPIRefusal(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'HTTP 405: Required status checks have not passed' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var response githubLandingMerge
	err := githubLandingAPI(t.Context(), &response, "PUT", "repos/example/repo/pulls/1/merge")
	var refusal *LandRefusal
	if !errors.As(err, &refusal) || refusal.Kind != LandRefusalProtected || !strings.Contains(refusal.Reason, "status checks") {
		t.Fatalf("refusal = %v", err)
	}
}
