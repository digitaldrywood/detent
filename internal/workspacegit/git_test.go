package workspacegit_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacegit"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// These tests run against real repositories rather than a fake git, because
// what this package has to get right is what git actually does: the byte layout
// of a -z status record, which exit code means "no upstream" rather than
// "broken", and whether a push that would overwrite history is refused. None of
// that is observable against a stub, and a stub that agreed with a wrong belief
// about it would pass while the surface committed a secret.

// requireGit skips a test on a machine with no git. The package already refuses
// to open a service without one, so there is nothing left to assert here.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

// gitAt runs one fixture command and fails the test when it does not succeed.
//
// Every invocation carries its own identity and default branch: what these
// tests assert must not depend on the git configuration of the machine they run
// on, and an author configured globally would make the authorship assertions
// pass for the wrong reason.
func gitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{
		"-c", "init.defaultBranch=main",
		"-c", "user.name=Fixture",
		"-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false",
	}, args...)...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

// write puts one file in the worktree.
func write(t *testing.T, root, relative, content string) {
	t.Helper()
	full := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newRepository builds a repository with one commit on main.
func newRepository(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitAt(t, root, "init", "-q", ".")
	write(t, root, "README.md", "# Fixture\n")
	gitAt(t, root, "add", "-A")
	gitAt(t, root, "commit", "-qm", "first")
	return root
}

// openService opens the service under test, with the project's fixed denylist
// plus whatever extra globs the case configures.
func openService(t *testing.T, root string, deny ...string) *workspacegit.Service {
	t.Helper()
	list, err := workspacesession.NewDenylist(deny)
	if err != nil {
		t.Fatal(err)
	}
	service, err := workspacegit.Open(t.Context(), root, list)
	if err != nil {
		t.Fatalf("open %s: %v", root, err)
	}
	return service
}

// person is the author a commit is made on behalf of, as the hub stamps it.
var person = workspacegit.Identity{Name: "Ada Lovelace", Email: "ada@example.invalid"}

// runner is the committer: the machine, never a person.
var runner = workspacegit.Identity{Name: "Detent runner (host-1)", Email: "runner@detent.invalid"}

func TestOpenRefusesWhatItMustNotWriteTo(t *testing.T) {
	t.Parallel()
	requireGit(t)
	repository := newRepository(t)
	write(t, repository, "sub/file.txt", "inside\n")

	cases := []struct {
		name string
		path func(*testing.T) string
	}{
		{
			name: "a directory that is not a repository",
			path: func(t *testing.T) string { return t.TempDir() },
		},
		{
			// A commit stages everything dirty, so a worktree that was only a
			// subdirectory of a repository would stage a person's unrelated
			// work elsewhere in it.
			name: "a subdirectory of a repository",
			path: func(*testing.T) string { return filepath.Join(repository, "sub") },
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			list, err := workspacesession.NewDenylist(nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workspacegit.Open(t.Context(), testCase.path(t), list); err == nil {
				t.Fatal("Open must refuse")
			} else if !errors.Is(err, workspacegit.ErrNotRepository) {
				t.Fatalf("Open refused with %v, want ErrNotRepository", err)
			}
		})
	}
}

func TestStatusReportsTheWorktree(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name  string
		setup func(*testing.T, string)
		want  workspacesession.GitStatus
	}{
		{
			name:  "clean",
			setup: func(*testing.T, string) {},
			want:  workspacesession.GitStatus{Branch: "main"},
		},
		{
			// An untracked file and a modified one are both dirty, and
			// --untracked-files=all is what makes the first of them count.
			name: "dirty",
			setup: func(t *testing.T, root string) {
				write(t, root, "README.md", "# Fixture, edited\n")
				write(t, root, "notes/new.txt", "untracked\n")
			},
			want: workspacesession.GitStatus{Branch: "main", Dirty: 2},
		},
		{
			name: "detached",
			setup: func(t *testing.T, root string) {
				gitAt(t, root, "checkout", "-q", "--detach", "HEAD")
			},
			want: workspacesession.GitStatus{Branch: "HEAD", Detached: true},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := newRepository(t)
			testCase.setup(t, root)
			status, err := openService(t, root).Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if status.HeadSHA == "" {
				t.Fatal("a repository with a commit must report a head sha")
			}
			status.HeadSHA = ""
			if status != testCase.want {
				t.Fatalf("status = %+v, want %+v", status, testCase.want)
			}
		})
	}
}

// TestStatusReportsDivergenceOnlyAgainstAnUpstream is the rule that keeps a
// reader from being shown "0 ahead" for a branch nobody could compare: without
// a tracking ref the counts are unknown, not zero, and Upstream is what says
// which of the two it is.
func TestStatusReportsDivergenceOnlyAgainstAnUpstream(t *testing.T) {
	t.Parallel()
	requireGit(t)

	t.Run("with an upstream", func(t *testing.T) {
		t.Parallel()
		root, _ := newRepositoryWithOrigin(t)
		write(t, root, "local.txt", "one commit ahead\n")
		gitAt(t, root, "add", "-A")
		gitAt(t, root, "commit", "-qm", "local")

		status, err := openService(t, root).Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !status.Upstream || status.Ahead != 1 || status.Behind != 0 {
			t.Fatalf("status = %+v, want upstream with ahead 1 and behind 0", status)
		}
		if status.Remote != "origin" {
			t.Fatalf("remote = %q, want origin", status.Remote)
		}
	})

	t.Run("without an upstream", func(t *testing.T) {
		t.Parallel()
		root := newRepository(t)
		status, err := openService(t, root).Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if status.Upstream || status.Ahead != 0 || status.Behind != 0 {
			t.Fatalf("status = %+v, want no upstream and no counts", status)
		}
		if status.Remote != "" {
			t.Fatalf("remote = %q, want none", status.Remote)
		}
	})
}

// newRepositoryWithOrigin builds a repository whose main branch tracks a bare
// origin beside it, which is the shape every push assertion needs.
func newRepositoryWithOrigin(t *testing.T) (root, origin string) {
	t.Helper()
	root = newRepository(t)
	base := filepath.Dir(root)
	origin = filepath.Join(base, "origin.git")
	gitAt(t, base, "init", "--bare", "-q", "origin.git")
	gitAt(t, root, "remote", "add", "origin", origin)
	gitAt(t, root, "push", "-q", "-u", "origin", "main")
	return root, origin
}

// TestCommitLeavesDeniedPathsOutOfTheCommit is section 18.4 applied to a write:
// a path the files channel refuses to show is a path this package refuses to
// stage, and it says which ones rather than dropping them quietly.
func TestCommitLeavesDeniedPathsOutOfTheCommit(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := newRepository(t)
	write(t, root, ".env", "TOKEN=super-secret\n")
	write(t, root, "secret.pem", "-----BEGIN PRIVATE KEY-----\n")
	write(t, root, "docs/README.md", "# Docs\n")

	committed, err := openService(t, root).Commit(t.Context(), "add docs", person, runner)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Files != 1 || committed.Commit == "" || committed.Branch != "main" {
		t.Fatalf("committed = %+v, want one file on main", committed)
	}
	if !slices.Equal(committed.Excluded, []string{".env", "secret.pem"}) {
		t.Fatalf("excluded = %v, want .env and secret.pem", committed.Excluded)
	}
	tree := strings.Split(gitAt(t, root, "show", "--name-only", "--format=", "HEAD"), "\n")
	if !slices.Contains(tree, "docs/README.md") {
		t.Fatalf("commit contains %v, want docs/README.md", tree)
	}
	for _, denied := range []string{".env", "secret.pem"} {
		if slices.Contains(tree, denied) {
			t.Fatalf("the commit carries %s: %v", denied, tree)
		}
	}
	// A denied path is left exactly as it was: still in the worktree, still
	// dirty, and visible to the person as something they will have to deal with
	// outside this surface.
	if status := gitAt(t, root, "status", "--porcelain"); !strings.Contains(status, ".env") || !strings.Contains(status, "secret.pem") {
		t.Fatalf("status after the commit = %q, want the denied paths still dirty", status)
	}
}

// TestCommitTakesADeniedPathOutOfTheIndex covers the case where something else
// staged the secret first. The index belongs to the worktree rather than to
// this service, and a plain commit records the index, so a denied path left in
// it would reach the commit and from there a remote.
func TestCommitTakesADeniedPathOutOfTheIndex(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := newRepository(t)
	write(t, root, ".env", "TOKEN=super-secret\n")
	write(t, root, "app.go", "package app\n")
	gitAt(t, root, "add", "-A", "-f")

	committed, err := openService(t, root).Commit(t.Context(), "add app", person, runner)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(committed.Excluded, []string{".env"}) {
		t.Fatalf("excluded = %v, want .env", committed.Excluded)
	}
	tree := strings.Split(gitAt(t, root, "show", "--name-only", "--format=", "HEAD"), "\n")
	if slices.Contains(tree, ".env") {
		t.Fatalf("a staged secret reached the commit: %v", tree)
	}
	if !slices.Contains(tree, "app.go") {
		t.Fatalf("commit contains %v, want app.go", tree)
	}
}

// TestCommitRecordsThePersonAndTheRunner is the attribution rule: a person
// authors and the machine commits. Recording the runner as the author would
// credit a person's change to the runner account, and recording the person as
// the committer would claim they ran this machine.
func TestCommitRecordsThePersonAndTheRunner(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root := newRepository(t)
	write(t, root, "app.go", "package app\n")

	if _, err := openService(t, root).Commit(t.Context(), "add app", person, runner); err != nil {
		t.Fatal(err)
	}
	identity := gitAt(t, root, "log", "-1", "--format=%an|%ae|%cn|%ce")
	want := person.Name + "|" + person.Email + "|" + runner.Name + "|" + runner.Email
	if identity != want {
		t.Fatalf("identity = %q, want %q", identity, want)
	}
}

// TestCommitCarriesWhatIsAlreadyInTheIndex covers the two records whose paths
// exist on neither side once git has staged them. Handing either of them to
// git add would fail on a pathspec that matches nothing and take the whole
// commit with it, so a worktree holding one staged deletion could not be
// committed from this surface at all. The name with a space in it is
// deliberate: it is what -z is for.
func TestCommitCarriesWhatIsAlreadyInTheIndex(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name  string
		stage func(*testing.T, string)
		files int
	}{
		{
			// A rename is one change to two paths, and recording only the new
			// one would leave the old name behind in the commit.
			name:  "a staged rename",
			stage: func(t *testing.T, root string) { gitAt(t, root, "mv", "old name.txt", "new name.txt") },
			files: 2,
		},
		{
			name:  "a staged deletion",
			stage: func(t *testing.T, root string) { gitAt(t, root, "rm", "-q", "old name.txt") },
			files: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := newRepository(t)
			write(t, root, "old name.txt", "carried\n")
			gitAt(t, root, "add", "-A")
			gitAt(t, root, "commit", "-qm", "second")
			testCase.stage(t, root)

			committed, err := openService(t, root).Commit(t.Context(), "index only", person, runner)
			if err != nil {
				t.Fatal(err)
			}
			if committed.Files != testCase.files {
				t.Fatalf("files = %d, want %d", committed.Files, testCase.files)
			}
			if status := gitAt(t, root, "status", "--porcelain"); status != "" {
				t.Fatalf("status after the commit = %q, want a clean worktree", status)
			}
		})
	}
}

func TestCommitRefusesWhenThereIsNothingToRecord(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{
			name:  "a clean worktree",
			setup: func(*testing.T, string) {},
		},
		{
			// Every dirty path being denied is not a clean worktree, and the
			// person is told which of the two it was.
			name: "a worktree whose every change is denied",
			setup: func(t *testing.T, root string) {
				write(t, root, ".env", "TOKEN=super-secret\n")
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := newRepository(t)
			before := gitAt(t, root, "rev-parse", "HEAD")
			testCase.setup(t, root)

			_, err := openService(t, root).Commit(t.Context(), "nothing", person, runner)
			if !errors.Is(err, workspacegit.ErrNothingToCommit) {
				t.Fatalf("commit failed with %v, want ErrNothingToCommit", err)
			}
			if workspacegit.Message(err) == "" {
				t.Fatal("the refusal must carry a sentence a person can read")
			}
			if after := gitAt(t, root, "rev-parse", "HEAD"); after != before {
				t.Fatal("an empty commit must not be made")
			}
		})
	}
}

func TestPushSendsTheBranchToItsRemote(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root, origin := newRepositoryWithOrigin(t)
	before := gitAt(t, origin, "rev-parse", "refs/heads/main")
	write(t, root, "app.go", "package app\n")
	gitAt(t, root, "add", "-A")
	gitAt(t, root, "commit", "-qm", "second")

	pushed, err := openService(t, root).Push(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	local := gitAt(t, root, "rev-parse", "HEAD")
	if pushed.Branch != "main" || pushed.Remote != "origin" || pushed.Commit != local {
		t.Fatalf("pushed = %+v, want main to origin at %s", pushed, local)
	}
	after := gitAt(t, origin, "rev-parse", "refs/heads/main")
	if after == before || after != local {
		t.Fatalf("origin/main = %s, want %s", after, local)
	}
}

func TestPushRefusesWhatItCannotSend(t *testing.T) {
	t.Parallel()
	requireGit(t)

	cases := []struct {
		name  string
		setup func(*testing.T) string
		want  error
	}{
		{
			// A detached HEAD is on no branch, so there is no branch to push,
			// and inventing one would push a person's work to a name they never
			// chose.
			name: "a detached head",
			setup: func(t *testing.T) string {
				root, _ := newRepositoryWithOrigin(t)
				gitAt(t, root, "checkout", "-q", "--detach", "HEAD")
				return root
			},
			want: workspacegit.ErrDetachedHead,
		},
		{
			name:  "a worktree with no remote",
			setup: func(t *testing.T) string { return newRepository(t) },
			want:  workspacegit.ErrNoRemote,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			root := testCase.setup(t)
			if _, err := openService(t, root).Push(t.Context()); !errors.Is(err, testCase.want) {
				t.Fatalf("push failed with %v, want %v", err, testCase.want)
			}
		})
	}
}

// TestPushNeverOverwritesTheRemote is why no command in this package is ever
// given a force flag. A branch that has diverged from its remote is refused by
// the remote, the person is shown git's own words for it, and somebody else's
// commits are still there afterwards.
func TestPushNeverOverwritesTheRemote(t *testing.T) {
	t.Parallel()
	requireGit(t)
	root, origin := newRepositoryWithOrigin(t)
	base := filepath.Dir(root)

	// Somebody else pushes first, so the remote holds a commit this worktree
	// has never seen.
	gitAt(t, base, "clone", "-q", origin, "other")
	other := filepath.Join(base, "other")
	write(t, other, "theirs.txt", "not ours\n")
	gitAt(t, other, "add", "-A")
	gitAt(t, other, "commit", "-qm", "theirs")
	gitAt(t, other, "push", "-q", "origin", "main")
	theirs := gitAt(t, origin, "rev-parse", "refs/heads/main")

	write(t, root, "ours.txt", "ours\n")
	gitAt(t, root, "add", "-A")
	gitAt(t, root, "commit", "-qm", "ours")

	_, err := openService(t, root).Push(t.Context())
	if err == nil {
		t.Fatal("a diverged push must fail rather than overwrite the remote")
	}
	if stderr := workspacegit.Stderr(err); !strings.Contains(stderr, "rejected") {
		t.Fatalf("stderr = %q, want git's own rejection", stderr)
	}
	if after := gitAt(t, origin, "rev-parse", "refs/heads/main"); after != theirs {
		t.Fatalf("origin/main = %s, want somebody else's commit %s still there", after, theirs)
	}
}

// TestIdentityValidateRefusesAForgedAuthor covers the line git writes:
// "Name <email> timestamp". A half carrying an angle bracket or a newline could
// close that line and open another, which is a commit attributed to somebody
// who never made one.
func TestIdentityValidateRefusesAForgedAuthor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		identity workspacegit.Identity
		valid    bool
	}{
		{name: "a person", identity: person, valid: true},
		{name: "the runner", identity: runner, valid: true},
		{name: "no email", identity: workspacegit.Identity{Name: "Ada"}},
		{name: "no name", identity: workspacegit.Identity{Email: "ada@example.invalid"}},
		{name: "blank name", identity: workspacegit.Identity{Name: "   ", Email: "ada@example.invalid"}},
		{
			name:     "a second identity in the name",
			identity: workspacegit.Identity{Name: "Ada <root@example.invalid> x", Email: "ada@example.invalid"},
		},
		{
			name:     "a newline in the name",
			identity: workspacegit.Identity{Name: "Ada\nauthor Root", Email: "ada@example.invalid"},
		},
		{
			name:     "a carriage return in the email",
			identity: workspacegit.Identity{Name: "Ada", Email: "ada@example.invalid\rx"},
		},
		{
			name:     "a NUL byte in the email",
			identity: workspacegit.Identity{Name: "Ada", Email: "ada@example.invalid\x00"},
		},
		{
			name:     "angle brackets in the email",
			identity: workspacegit.Identity{Name: "Ada", Email: "<ada@example.invalid>"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.identity.Validate()
			if testCase.valid {
				if err != nil {
					t.Fatalf("Validate = %v, want the identity accepted", err)
				}
				return
			}
			if !errors.Is(err, workspacegit.ErrInvalidIdentity) {
				t.Fatalf("Validate = %v, want ErrInvalidIdentity", err)
			}
		})
	}
}
