package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalGitLandChangeViaGitHub(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name        string
		mergeError  string
		wantRefusal string
		moved       bool
		method      string
		pullState   string
		reworked    bool
		external    bool
		pullError   string
		lookupError string
	}{
		{name: "merges the reviewed head", method: "merge"},
		{name: "uses the policy squash method", method: "squash"},
		{name: "uses the policy rebase method", method: "rebase"},
		{name: "reports protected merge refusal", method: "merge", mergeError: "HTTP 405: Branch protection requires reviews", wantRefusal: LandRefusalProtected},
		{name: "reuses an open PR", method: "merge", pullState: "open"},
		{name: "records an already merged reviewed head", method: "merge", pullState: "merged"},
		{name: "ignores an older merged PR", method: "merge", pullState: "older"},
		{name: "publishes a reworked branch despite a stale list head", method: "squash", reworked: true, pullState: "stale"},
		{name: "atomic merge rejects a genuinely moved head", method: "squash", reworked: true, pullState: "stale", moved: true, wantRefusal: LandRefusalHeadMoved},
		{name: "atomic merge rejects a closed PR", method: "squash", pullState: "open", mergeError: "gh: Pull Request is closed (HTTP 405)", wantRefusal: LandRefusalHeadMoved},
		{name: "reuses the explicit external PR", method: "merge", external: true},
		{name: "external moved head is never overwritten", method: "merge", external: true, pullError: "head", wantRefusal: LandRefusalHeadMoved},
		{name: "external branch must match", method: "merge", external: true, pullError: "branch", wantRefusal: LandRefusalHeadMoved},
		{name: "external repository must match", method: "merge", external: true, pullError: "repository", wantRefusal: LandRefusalProtected},
		{name: "external fork must match", method: "merge", external: true, pullError: "fork", wantRefusal: LandRefusalProtected},
		{name: "external base must match", method: "merge", external: true, pullError: "base", wantRefusal: LandRefusalProtected},
		{name: "external authentication denied", method: "merge", external: true, lookupError: "HTTP 401: Bad credentials", wantRefusal: LandRefusalProtected},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLandingFixture(t)
			base := fixture.remoteMain(t)
			repository := "https://github.com/example/repo"
			// Keep the reviewed URL GitHub-shaped while git sends to the local
			// bare repository. Neither test touches a real GitHub repository.
			runGit(t, fixture.source, "config", "url.file://"+fixture.remote+".insteadOf", repository+".git")
			runGit(t, fixture.source, "remote", "set-url", "origin", repository+".git")
			previous := base
			externalHead := fixture.head
			if test.external {
				runGit(t, fixture.source, "push", "origin", fixture.head+":refs/heads/"+fixture.info.Branch)
				if test.pullError == "head" {
					externalHead = previous
					runGit(t, fixture.remote, "update-ref", "refs/heads/"+fixture.info.Branch, externalHead)
				}
			}
			if test.reworked {
				tree := strings.TrimSpace(runGit(t, fixture.source, "rev-parse", fixture.head+"^{tree}"))
				previous = strings.TrimSpace(runGit(t, fixture.source, "commit-tree", tree, "-p", base, "-m", "Previous attempt"))
				runGit(t, fixture.source, "push", "origin", previous+":refs/heads/"+fixture.info.Branch)
			}
			bin := installLandingGitHubCLI(t)
			t.Setenv("TEST_BARE_REPOSITORY", fixture.remote)
			t.Setenv("TEST_REVIEWED_HEAD", fixture.head)
			t.Setenv("TEST_OLD_HEAD", previous)
			t.Setenv("TEST_PULL_STATE", test.pullState)
			t.Setenv("TEST_ATTEMPT_BRANCH", fixture.info.Branch)
			t.Setenv("TEST_MOVE_HEAD", fmt.Sprint(test.moved))
			callsPath := filepath.Join(bin, "calls")
			t.Setenv("TEST_GH_CALLS", callsPath)
			t.Setenv("TEST_MERGE_REFUSED", test.mergeError)
			t.Setenv("TEST_EXTERNAL_PULL_ERROR", test.pullError)
			t.Setenv("TEST_LOOKUP_REFUSED", test.lookupError)
			options := LandOptions{
				HeadSHA: fixture.head, Method: test.method, Repository: repository,
				Message: "Review this change\n\nNative Change Request", PushAttemptBranch: true,
			}
			if test.external {
				options.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: repository + "/pull/7"}
			}
			result, err := fixture.backend.LandChangeViaGitHub(context.Background(), fixture.info, fixture.issue, options)
			calls, readErr := os.ReadFile(callsPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			wantCreate := test.pullState != "open" && test.pullState != "stale" && test.pullState != "merged"
			wantMerge := test.pullState != "merged"
			if test.external {
				wantCreate = false
				wantMerge = test.wantRefusal == ""
				if !strings.Contains(string(calls), "GET repos/example/repo/pulls/7") || strings.Contains(string(calls), "state=all") {
					t.Fatalf("external PR was not read directly: %s", calls)
				}
			}
			if strings.Contains(string(calls), "--method POST") != wantCreate || strings.Contains(string(calls), "--method PUT") != wantMerge {
				t.Fatalf("unexpected PR operations: %s", calls)
			}
			if wantMerge && (!strings.Contains(string(calls), "merge_method="+test.method) || !strings.Contains(string(calls), "sha="+fixture.head)) {
				t.Fatalf("merge did not name the approved method and head: %s", calls)
			}
			if test.wantRefusal != "" {
				var refusal *LandRefusal
				if !errors.As(err, &refusal) || refusal.Kind != test.wantRefusal || fixture.remoteMain(t) != base || result.MergeSHA != "" {
					t.Fatalf("merge refusal = %#v, %v; base = %s", result, err, fixture.remoteMain(t))
				}
				if test.moved && !strings.Contains(refusal.Reason, "Head branch was modified") {
					t.Fatalf("head refusal did not come from the atomic merge: %v", err)
				}
				if test.external {
					published := strings.TrimSpace(runGit(t, fixture.remote, "rev-parse", "refs/heads/"+fixture.info.Branch))
					if published != externalHead {
						t.Fatalf("external branch was rewritten: %s", published)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.MergeSHA != fixture.head || result.BaseRef != "main" || result.BaseBefore != base || result.Method != test.method || result.AttemptBranchPushed != !test.external || fixture.remoteMain(t) != fixture.head {
				t.Fatalf("GitHub landing = %#v; base = %s", result, fixture.remoteMain(t))
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
	installLandingGitHubCLI(t)
	for _, test := range []struct {
		name, message, kind string
		quota               bool
	}{
		{name: "required checks", message: "HTTP 405: Required status checks have not passed", kind: LandRefusalProtected},
		{name: "required reviews", message: "HTTP 405: Branch protection requires reviews", kind: LandRefusalProtected},
		{name: "protected base", message: "HTTP 405: Protected branch update failed", kind: LandRefusalProtected},
		{name: "unspecified refusal", message: "HTTP 405: Method Not Allowed", kind: LandRefusalProtected},
		{name: "explicit merge conflict", message: "gh: Merge conflict (HTTP 405)", kind: LandRefusalConflict},
		{name: "unmergeable pull request", message: `{"message":"Pull Request is not mergeable","status":"405"}
gh: Pull Request is not mergeable (HTTP 405)`, kind: LandRefusalConflict},
		{name: "authentication", message: "HTTP 401: Bad credentials", kind: LandRefusalProtected},
		{name: "authoritative moved head", message: "gh: Head branch was modified. Review and try the merge again. (HTTP 409)", kind: LandRefusalHeadMoved},
		{name: "authoritative closed PR", message: "gh: Pull Request is closed (HTTP 405)", kind: LandRefusalHeadMoved},
		{name: "authoritative PR is not open", message: "gh: Pull Request is not open (HTTP 405)", kind: LandRefusalHeadMoved},
		{name: "primary quota never becomes a conflict", message: "HTTP 403: API rate limit exceeded", quota: true},
		{name: "secondary quota never becomes a conflict", message: "HTTP 429: You have exceeded a secondary rate limit", quota: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("TEST_MERGE_REFUSED", test.message)
			var response githubLandingMerge
			err := githubLandingAPI(t.Context(), &response, "PUT", "repos/example/repo/pulls/1/merge")
			var refusal *LandRefusal
			typed := errors.As(err, &refusal)
			if err == nil || !strings.Contains(err.Error(), test.message) || response.Merged {
				t.Fatalf("refusal = %v, response = %#v", err, response)
			}
			if test.quota {
				if typed && (refusal.Kind == LandRefusalConflict || refusal.Kind == LandRefusalHeadMoved) {
					t.Fatalf("quota became a conflict or head refusal: %v", err)
				}
				return
			}
			if !typed || refusal.Kind != test.kind {
				t.Fatalf("refusal = %v, response = %#v, want kind %q", err, response, test.kind)
			}
		})
	}
}

// Run a copy of this test executable as gh so the fixture works without a
// shell interpreter, including on Windows hosts with a real gh on PATH.
func installLandingGitHubCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "GitHub CLI fixture")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fixture := filepath.Join(bin, name)
	if err := os.WriteFile(fixture, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_LANDING_GH_HELPER", "1")
	t.Setenv("TEST_GH_CALLS", "")
	resolved, err := exec.LookPath("gh")
	if err != nil || resolved != fixture {
		t.Fatalf("gh fixture resolved to %q, want %q: %v", resolved, fixture, err)
	}
	return bin
}

func landingGitHubCLIHelper() int {
	args := os.Args[1:]
	if len(args) < 4 || args[0] != "api" || args[1] != "--method" {
		fmt.Fprintf(os.Stderr, "unexpected gh arguments: %q\n", args)
		return 1
	}
	if callsPath := os.Getenv("TEST_GH_CALLS"); callsPath != "" {
		calls, err := os.OpenFile(callsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		_, writeErr := fmt.Fprintln(calls, strings.Join(args, " "))
		closeErr := calls.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	head := os.Getenv("TEST_REVIEWED_HEAD")
	updateBase := func() error {
		cmd := exec.CommandContext(context.Background(), "git", "--git-dir", os.Getenv("TEST_BARE_REPOSITORY"), "update-ref", "refs/heads/main", head)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	pull := fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s"},"base":{"ref":"main"}}`, head)
	var response string
	switch args[2] {
	case "GET":
		if !strings.Contains(args[3], "?") {
			if refusal := os.Getenv("TEST_LOOKUP_REFUSED"); refusal != "" {
				fmt.Fprintln(os.Stderr, refusal)
				return 1
			}
			headRef, baseRef, headRepo, baseRepo := os.Getenv("TEST_ATTEMPT_BRANCH"), "main", "example/repo", "example/repo"
			switch os.Getenv("TEST_EXTERNAL_PULL_ERROR") {
			case "head":
				head = os.Getenv("TEST_OLD_HEAD")
			case "branch":
				headRef = "another-branch"
			case "base":
				baseRef = "another-base"
			case "repository":
				baseRepo = "another/repo"
			case "fork":
				headRepo = "another/repo"
			}
			response = fmt.Sprintf(`{"number":7,"state":"open","head":{"sha":"%s","ref":"%s","repo":{"full_name":"%s"}},"base":{"ref":"%s","repo":{"full_name":"%s"}}}`, head, headRef, headRepo, baseRef, baseRepo)
			break
		}
		switch os.Getenv("TEST_PULL_STATE") {
		case "open", "stale":
			cmd := exec.CommandContext(context.Background(), "git", "--git-dir", os.Getenv("TEST_BARE_REPOSITORY"), "rev-parse", "refs/heads/"+os.Getenv("TEST_ATTEMPT_BRANCH"))
			published, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(published)) != head {
				fmt.Fprintf(os.Stderr, "list read preceded reviewed head publication: %s, %v\n", published, err)
				return 1
			}
			if os.Getenv("TEST_PULL_STATE") == "stale" {
				pull = strings.ReplaceAll(pull, head, os.Getenv("TEST_OLD_HEAD"))
			}
			response = "[" + pull + "]"
		case "merged":
			if err := updateBase(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			response = fmt.Sprintf(`[{"number":7,"state":"closed","merged_at":"2026-09-29T00:00:00Z","merge_commit_sha":"%s","head":{"sha":"%s"},"base":{"ref":"main"}}]`, head, head)
		case "older":
			oldHead := os.Getenv("TEST_OLD_HEAD")
			response = fmt.Sprintf(`[{"number":6,"state":"closed","merged_at":"2026-09-28T00:00:00Z","merge_commit_sha":"%s","head":{"sha":"%s"},"base":{"ref":"main"}}]`, oldHead, oldHead)
		default:
			response = "[]"
		}
	case "POST":
		response = pull
	case "PUT":
		if branch := os.Getenv("TEST_ATTEMPT_BRANCH"); branch != "" {
			if os.Getenv("TEST_MOVE_HEAD") == "true" {
				cmd := exec.CommandContext(context.Background(), "git", "--git-dir", os.Getenv("TEST_BARE_REPOSITORY"), "update-ref", "refs/heads/"+branch, os.Getenv("TEST_OLD_HEAD"), head)
				if err := cmd.Run(); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return 1
				}
			}
			cmd := exec.CommandContext(context.Background(), "git", "--git-dir", os.Getenv("TEST_BARE_REPOSITORY"), "rev-parse", "refs/heads/"+branch)
			current, err := cmd.Output()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			guarded := false
			for _, arg := range args[4:] {
				if arg == "sha="+strings.TrimSpace(string(current)) {
					guarded = true
				}
			}
			if !guarded {
				fmt.Fprintln(os.Stderr, "gh: Head branch was modified. Review and try the merge again. (HTTP 409)")
				return 1
			}
		}
		if refusal := os.Getenv("TEST_MERGE_REFUSED"); refusal != "" {
			fmt.Fprintln(os.Stderr, refusal)
			return 1
		}
		if err := updateBase(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		response = fmt.Sprintf(`{"merged":true,"sha":"%s"}`, head)
	default:
		fmt.Fprintf(os.Stderr, "unexpected gh method: %s\n", args[2])
		return 1
	}
	if _, err := fmt.Fprintln(os.Stdout, response); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
