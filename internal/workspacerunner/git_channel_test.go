package workspacerunner_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The git channel is tested end to end through the relay for the same reason
// the files channel is: what the session has to get right is the order of its
// gates. A commit refused for a lost lease, a commit refused on a read-only
// workspace and a commit made under the wrong author are all the same method
// call with different frames around it, and only the frames show the
// difference.

// gitFixture runs one fixture command against a repository, with an identity
// and a default branch of its own so nothing here depends on the git
// configuration of the machine the tests run on.
func gitFixture(t *testing.T, dir string, args ...string) string {
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

// repositoryWorktree builds a worktree the git channel can serve: one commit
// on main, a source file to commit and a secret the denylist refuses.
func repositoryWorktree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := filepath.Join(t.TempDir(), "worktree")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFixture(t, root, "init", "-q", ".")
	writeWorktreeFile(t, root, "README.md", "# Fixture\n")
	gitFixture(t, root, "add", "-A")
	gitFixture(t, root, "commit", "-qm", "first")
	writeWorktreeFile(t, root, "app.go", "package app\n")
	writeWorktreeFile(t, root, ".env", "TOKEN=super-secret\n")
	return root
}

func writeWorktreeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, relative), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// gitFrame builds one person-originated git frame. The actor is the hub's stamp
// and never the client's, which is what makes it usable as a commit author.
func gitFrame(t *testing.T, kind, stream string, request workspacesession.GitRequest, actor *workspacesession.Actor) workspacesession.Frame {
	t.Helper()
	payload, err := workspacesession.Encode(request)
	if err != nil {
		t.Fatal(err)
	}
	return workspacesession.Frame{
		Channel: workspacesession.ChannelGit, Stream: stream, Type: kind, Payload: payload, Actor: actor,
	}
}

// person is the actor the hub stamps on a frame from Ada's browser.
func person() *workspacesession.Actor {
	return &workspacesession.Actor{
		PrincipalID: "pr_1", Subject: "ada", ConnectionID: "conn",
		Name: "Ada Lovelace", Email: "ada@example.invalid",
	}
}

// reports copies the heartbeats this hub has received.
func (h *scriptedHub) reports() []hubclient.WorkspaceHeartbeatRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.heartbeats)
}

func (f *sessionFixture) errorAnswer(t *testing.T) workspacesession.ErrorPayload {
	t.Helper()
	answer := f.receive(t)
	if answer.Type != workspacesession.TypeError {
		t.Fatalf("answer = %+v, want an error frame", answer)
	}
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(answer.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestSessionServesTheGitChannel(t *testing.T) {
	t.Parallel()
	root := repositoryWorktree(t)
	f := startSessionWith(t, root, nil, nil)

	f.send(t, gitFrame(t, workspacesession.TypeGitStatus, "conn:1", workspacesession.GitRequest{}, person()))
	answer := f.receive(t)
	if answer.Type != workspacesession.TypeGitStatus {
		t.Fatalf("status answered %+v", answer)
	}
	var status workspacesession.GitStatus
	if err := json.Unmarshal(answer.Payload, &status); err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" || status.Detached || status.Dirty != 2 {
		t.Fatalf("status = %+v, want main with two dirty paths", status)
	}
	// A branch with no tracking ref reports no counts: they are unknown rather
	// than zero, and Upstream is what says which.
	if status.Upstream || status.Ahead != 0 || status.Behind != 0 {
		t.Fatalf("status = %+v, want no upstream", status)
	}

	f.send(t, gitFrame(t, workspacesession.TypeGitCommit, "conn:1",
		workspacesession.GitRequest{Message: "add app"}, person()))
	answer = f.receive(t)
	if answer.Type != workspacesession.TypeGitCommitted {
		t.Fatalf("commit answered %+v", answer)
	}
	var committed workspacesession.GitCommitted
	if err := json.Unmarshal(answer.Payload, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Files != 1 || committed.Branch != "main" || committed.Commit == "" {
		t.Fatalf("committed = %+v, want one file on main", committed)
	}
	// The denylist travels with the surface: the secret is reported as excluded
	// rather than committed, and rather than dropped without a word.
	if !slices.Equal(committed.Excluded, []string{".env"}) {
		t.Fatalf("excluded = %v, want .env", committed.Excluded)
	}
	if tree := gitFixture(t, root, "show", "--name-only", "--format=", "HEAD"); tree != "app.go" {
		t.Fatalf("the commit carries %q, want app.go alone", tree)
	}
	// A person authors and the machine commits, so neither can be mistaken for
	// the other in the history.
	identity := gitFixture(t, root, "log", "-1", "--format=%an|%ae|%cn|%ce")
	want := "Ada Lovelace|ada@example.invalid|Detent runner (runner-host)|runner@detent.invalid"
	if identity != want {
		t.Fatalf("identity = %q, want %q", identity, want)
	}
}

// TestSessionReportsGitOnlyForARepository is the distinction between what this
// runner can serve in principle and what one worktree can actually do. The
// claim gate matches on the first; the workspace resource has to carry the
// second, because the header enables its git group from it.
func TestSessionReportsGitOnlyForARepository(t *testing.T) {
	t.Parallel()
	if !workspacerunner.Capabilities(workspacerunner.DefaultSupport()).Git {
		t.Fatal("the runner serves the git channel in principle")
	}

	t.Run("a repository", func(t *testing.T) {
		t.Parallel()
		root := repositoryWorktree(t)
		f := startSessionWith(t, root, nil, nil)
		reports := f.hub.reports()
		if len(reports) == 0 {
			t.Fatal("the session must heartbeat before it serves")
		}
		if !reports[0].Capabilities.Git || !reports[0].Capabilities.Files {
			t.Fatalf("capabilities = %+v, want files and git", reports[0].Capabilities)
		}
		// The path and the host are what the header's Open picker needs to tell
		// a worktree on the reader's own machine from one somewhere else.
		if reports[0].WorktreePath != root || reports[0].MachineHostname != "runner-host" {
			t.Fatalf("report = %q on %q, want %q on runner-host",
				reports[0].WorktreePath, reports[0].MachineHostname, root)
		}
	})

	t.Run("a worktree that is not a repository", func(t *testing.T) {
		t.Parallel()
		// A worktree that is not a repository is not a failed workspace: the
		// files channel serves it and the git group is disabled with a reason,
		// which is better for the person than losing the whole workspace.
		f := startSession(t, nil)
		reports := f.hub.reports()
		if len(reports) == 0 {
			t.Fatal("the session must heartbeat before it serves")
		}
		if reports[0].Capabilities.Git {
			t.Fatalf("capabilities = %+v, want no git for a worktree that is not a repository", reports[0].Capabilities)
		}
		f.send(t, gitFrame(t, workspacesession.TypeGitStatus, "conn:1", workspacesession.GitRequest{}, person()))
		if payload := f.errorAnswer(t); payload.Code != workspacesession.CodeUnsupported {
			t.Fatalf("status answered %+v, want unsupported", payload)
		}
	})
}

// TestSessionRefusesAGitWriteOnAReadOnlyWorkspace is section 18.1's rule that a
// person may look at the worktree the model is editing and may not type into
// it. Status still answers: a reader who may only read still wants the branch
// name the header shows.
func TestSessionRefusesAGitWriteOnAReadOnlyWorkspace(t *testing.T) {
	t.Parallel()
	root := repositoryWorktree(t)
	f := startSessionWith(t, root, func(checkout *hubclient.WorkspaceCheckout) {
		checkout.ReadOnly = true
		checkout.Worktree = workspacesession.WorktreeRetained
	}, nil)

	for _, kind := range []string{workspacesession.TypeGitCommit, workspacesession.TypeGitPush} {
		f.send(t, gitFrame(t, kind, "conn:1", workspacesession.GitRequest{Message: "add app"}, person()))
		if payload := f.errorAnswer(t); payload.Code != workspacesession.CodeReadOnly {
			t.Fatalf("%s answered %+v, want read_only", kind, payload)
		}
	}
	if status := gitFixture(t, root, "status", "--porcelain"); !strings.Contains(status, "app.go") {
		t.Fatalf("status = %q, want the refused change still uncommitted", status)
	}

	f.send(t, gitFrame(t, workspacesession.TypeGitStatus, "conn:1", workspacesession.GitRequest{}, person()))
	if answer := f.receive(t); answer.Type != workspacesession.TypeGitStatus {
		t.Fatalf("status answered %+v, want a read to still be served", answer)
	}
}

// TestSessionDropsAGitFrameAfterLeaseLoss is the rule that makes it safe for a
// worktree to be written by exactly one generation at a time. It matters more
// here than on the files channel: a read under a lost lease shows a person
// something stale, and a commit under one writes into a worktree another runner
// now owns.
func TestSessionDropsAGitFrameAfterLeaseLoss(t *testing.T) {
	t.Parallel()
	root := repositoryWorktree(t)
	expired := time.Now()
	f := startSessionWith(t, root, nil, func(config *workspacerunner.Config) {
		config.Now = func() time.Time { return expired }
	})
	expired = expired.Add(workspacesession.LeaseTTL + time.Minute)

	f.send(t, gitFrame(t, workspacesession.TypeGitCommit, "conn:1",
		workspacesession.GitRequest{Message: "add app"}, person()))
	if payload := f.errorAnswer(t); payload.Code != workspacesession.CodeStaleExecution {
		t.Fatalf("a commit after lease loss answered %+v, want stale_execution", payload)
	}
	if head := gitFixture(t, root, "log", "-1", "--format=%s"); head != "first" {
		t.Fatalf("HEAD = %q, want no commit made under a lost lease", head)
	}
}

func TestSessionRefusesAGitFrameItCannotActOn(t *testing.T) {
	t.Parallel()
	root := repositoryWorktree(t)

	cases := []struct {
		name  string
		frame func(*testing.T) workspacesession.Frame
		code  string
	}{
		{
			name: "an unknown type",
			frame: func(*testing.T) workspacesession.Frame {
				return workspacesession.Frame{
					Channel: workspacesession.ChannelGit, Stream: "conn:1", Type: "amend", Actor: person(),
				}
			},
			code: workspacesession.CodeUnknownFrame,
		},
		{
			// Committing under the runner's own identity would attribute a
			// person's change to the machine, and a commit cannot be
			// re-attributed afterwards.
			name: "a commit with no actor",
			frame: func(t *testing.T) workspacesession.Frame {
				return gitFrame(t, workspacesession.TypeGitCommit, "conn:1",
					workspacesession.GitRequest{Message: "add app"}, nil)
			},
			code: workspacesession.CodeForbidden,
		},
		{
			name: "a commit whose actor has no email",
			frame: func(t *testing.T) workspacesession.Frame {
				return gitFrame(t, workspacesession.TypeGitCommit, "conn:1",
					workspacesession.GitRequest{Message: "add app"},
					&workspacesession.Actor{PrincipalID: "pr_1", Name: "Ada Lovelace"})
			},
			code: workspacesession.CodeForbidden,
		},
		{
			name: "a commit with no message",
			frame: func(t *testing.T) workspacesession.Frame {
				return gitFrame(t, workspacesession.TypeGitCommit, "conn:1",
					workspacesession.GitRequest{Message: "   "}, person())
			},
			code: workspacesession.CodeInvalidFrame,
		},
		{
			name: "a payload that does not parse",
			frame: func(*testing.T) workspacesession.Frame {
				return workspacesession.Frame{
					Channel: workspacesession.ChannelGit, Stream: "conn:1",
					Type: workspacesession.TypeGitCommit, Payload: json.RawMessage(`"not an object"`),
					Actor: person(),
				}
			},
			code: workspacesession.CodeInvalidFrame,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := startSessionWith(t, root, nil, nil)
			f.send(t, testCase.frame(t))
			if payload := f.errorAnswer(t); payload.Code != testCase.code {
				t.Fatalf("answer = %+v, want %s", payload, testCase.code)
			}
		})
	}
}

// TestSessionAnswersAFailedGitCommandWithGitsOwnWords is why git_failed is not
// a refusal. The request was allowed and git said no, and a person acting on
// that needs what git said rather than a paraphrase of it.
func TestSessionAnswersAFailedGitCommandWithGitsOwnWords(t *testing.T) {
	t.Parallel()
	root := repositoryWorktree(t)
	// A remote that is not there: the push is allowed, reaches git, and fails
	// with something only git can phrase.
	gitFixture(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))
	f := startSessionWith(t, root, nil, nil)

	f.send(t, gitFrame(t, workspacesession.TypeGitPush, "conn:1", workspacesession.GitRequest{}, person()))
	payload := f.errorAnswer(t)
	if payload.Code != workspacesession.CodeGitFailed {
		t.Fatalf("push answered %+v, want git_failed", payload)
	}
	if payload.Message == "" {
		t.Fatal("a git_failed answer must say what was attempted")
	}
	if !strings.Contains(payload.Stderr, "repository") {
		t.Fatalf("stderr = %q, want git's own words about the remote", payload.Stderr)
	}
}

// requires: ["files","git"] has to be bindable, and it was not.
//
// The bind is what returns the checkout instructions, so the worktree does not
// exist until after it; a bind that reported only what the prepared worktree
// can do therefore reported git: false for every workspace, the hub's own bind
// check refused every workspace that asked for git, and the runner re-claimed
// it until the request timed out. The workspace never failed and never became
// ready, which is the worst shape such a bug can have: the surface simply
// waits, and no reason is ever recorded anywhere.
//
// So the bind reports the build's answer and the first heartbeat narrows it to
// this worktree. Both halves are pinned here, because fixing either alone
// reintroduces the other bug: bind with the worktree's answer and nothing
// binds; heartbeat with the build's answer and the header enables a git group
// over a worktree that is not a repository.
func TestGitCapabilityIsClaimedAtBindAndNarrowedByTheHeartbeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// repository decides whether the prepared worktree is a git repository.
		repository bool
		// wantHeartbeatGit is what the workspace resource must end up carrying.
		wantHeartbeatGit bool
	}{
		{
			name:             "a git worktree keeps the capability it bound with",
			repository:       true,
			wantHeartbeatGit: true,
		},
		{
			// The whole point of narrowing: this workspace is still served and
			// its files channel still works; only the git group is disabled.
			name:             "a worktree that is not a repository still binds, and drops git",
			repository:       false,
			wantHeartbeatGit: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if test.repository {
				root = repositoryWorktree(t)
			}
			fixture := startSessionWith(t, root, func(checkout *hubclient.WorkspaceCheckout) {
				checkout.Requires = []string{
					workspacesession.CapabilityFiles, workspacesession.CapabilityGit,
				}
			}, nil)

			// The bind claims what the build implements, which is the same kind
			// of fact the claim gate already matched against this runner's own
			// heartbeat.
			bound := fixture.hub.bindRequests()
			if len(bound) != 1 {
				t.Fatalf("bind calls = %d, want exactly one", len(bound))
			}
			if !bound[0].Capabilities.Git {
				t.Fatal("the bind must claim git, or the hub refuses every workspace that requires it")
			}
			if !bound[0].Capabilities.Satisfies([]string{
				workspacesession.CapabilityFiles, workspacesession.CapabilityGit,
			}) {
				t.Fatalf("bind capabilities = %+v, want them to satisfy files and git", bound[0].Capabilities)
			}

			// And the first heartbeat says what this worktree can actually do.
			var reported []hubclient.WorkspaceHeartbeatRequest
			deadline := time.Now().Add(10 * time.Second)
			for {
				reported = fixture.hub.reports()
				if len(reported) > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(reported) == 0 {
				t.Fatal("the session never heartbeated, so the resource never learned what the worktree can do")
			}
			if got := reported[0].Capabilities.Git; got != test.wantHeartbeatGit {
				t.Fatalf("first heartbeat git = %v, want %v", got, test.wantHeartbeatGit)
			}
			if !reported[0].Capabilities.Files {
				t.Fatal("the files channel is served whether or not the worktree is a repository")
			}
			// The path and the host ride on the same beat, because the Open
			// picker has nothing to open without them (section 18.12).
			if reported[0].WorktreePath != root {
				t.Fatalf("worktree_path = %q, want %q", reported[0].WorktreePath, root)
			}
			if reported[0].MachineHostname != "runner-host" {
				t.Fatalf("machine_hostname = %q, want the runner's host", reported[0].MachineHostname)
			}
		})
	}
}

// bindRequests copies the bind requests this hub has received.
func (h *scriptedHub) bindRequests() []hubclient.WorkspaceBindRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.binds)
}

// heartbeatRequests copies the heartbeats this hub has received. The first one
// is the narrowing: a session reports the build's answer at bind, because no
// worktree exists yet, and the per-worktree answer on the ready beat.
func (h *scriptedHub) heartbeatRequests() []hubclient.WorkspaceHeartbeatRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.heartbeats)
}
