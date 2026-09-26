// Package workspacegit is the runner's git service for one workspace worktree
// (decisions section 18.13): the status the header shows, the commit a person
// asks for and the push that follows.
//
// It is the only workspace surface that writes, and every rule that follows
// from that is written down here rather than left to the caller. Three of them
// carry the weight:
//
//   - The denylist of section 18.4 applies to a commit exactly as it applies
//     to a read. A path the files channel refuses to show is a path this
//     package refuses to stage, and it reports which ones it refused instead
//     of dropping them quietly: a person who expected a file in the commit has
//     to be able to see why it is not in it.
//   - A person authors and the runner commits. The author comes from the hub's
//     stamp on the frame and the committer is this machine, so no commit can
//     attribute a person's change to the runner account, and none can
//     attribute the machine's work to a person.
//   - Nothing here may wait for a human. Every command runs with credential
//     prompting turned off, with no standard input, and under a timeout,
//     because a git process blocked on a passphrase would hold the relay's
//     read loop for as long as the person's patience lasts.
//
// No command is ever given a force flag. A push that would overwrite history
// on the remote is refused by the remote and reported with git's own words,
// which is what a person acting on a rejected push needs to see.
package workspacegit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// ErrNoGit reports that this runner has no git binary on PATH. It is not a
// failure of the workspace: the runner serves the files channel and leaves the
// git capability unreported, and the header disables the git group with a
// reason.
var ErrNoGit = errors.New("workspacegit: no git binary on PATH")

// ErrNotRepository reports a worktree that is not the top level of a git
// working tree.
var ErrNotRepository = errors.New("workspacegit: not a git working tree")

// ErrNothingToCommit reports a commit with nothing to record: a clean worktree,
// or one whose every dirty path the denylist refuses.
var ErrNothingToCommit = errors.New("workspacegit: nothing to commit")

// ErrDetachedHead reports a push with no branch to push.
var ErrDetachedHead = errors.New("workspacegit: HEAD is not on a branch")

// ErrNoRemote reports a push with nowhere to push to.
var ErrNoRemote = errors.New("workspacegit: no remote to push to")

// ErrInvalidIdentity reports an author or committer git could not use, or one
// that could forge a second identity into a commit.
var ErrInvalidIdentity = errors.New("workspacegit: invalid git identity")

// Error is a failure in the git channel's own vocabulary.
//
// Code is set when the failure maps onto a relay code the caller should answer
// with, and empty when git itself ran and said no: the relay has one code for
// that case (git_failed) and the caller reaches it through Stderr rather than
// through a code here. Message is the sentence a person reads, and it is on the
// error rather than in a table at the caller because "the worktree is clean"
// and "the push was refused" are not the same sentence and neither is derivable
// from a code.
type Error struct {
	Code    string
	Message string
	// Stderr is what git wrote, bounded by maxStderrBytes.
	Stderr string
	cause  error
}

func (e *Error) Error() string {
	parts := []string{}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.cause != nil {
		parts = append(parts, e.cause.Error())
	}
	if e.Stderr != "" {
		parts = append(parts, e.Stderr)
	}
	return strings.Join(parts, ": ")
}

func (e *Error) Unwrap() error { return e.cause }

// refuse builds a failure the caller answers with a relay code.
func refuse(code, message string, cause error) error {
	return &Error{Code: code, Message: message, cause: cause}
}

// fail builds a failure the caller answers with git_failed, carrying whatever
// git wrote to stderr.
func fail(message, stderr string, cause error) error {
	return &Error{Message: message, Stderr: stderr, cause: cause}
}

// ErrorCode reports the relay code for an error, or the empty string when the
// error is not one of ours or names no code.
func ErrorCode(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

// Message reports the sentence that goes with an error, or the empty string
// when the error is not one of ours.
func Message(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Message
	}
	return ""
}

// Stderr reports what git wrote for a failed command, so the caller can put it
// in the git_failed frame. A person acting on a rejected push needs git's own
// words rather than a paraphrase of them.
func Stderr(err error) string {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Stderr
	}
	return ""
}

// Command timeouts. They are the only thing between a git process that will
// not finish and a relay read loop that answers nothing else while it waits,
// so every command has one.
const (
	// readTimeout bounds the commands that only inspect the worktree. They
	// touch no network, so anything this slow is a filesystem in trouble
	// rather than work in progress.
	readTimeout = 15 * time.Second
	// writeTimeout bounds staging and the commit. It is longer because the
	// commit writes an object for every staged path and runs whatever
	// pre-commit hook the project has.
	writeTimeout = 60 * time.Second
	// pushTimeout bounds the one command that talks to a remote. It is the
	// longest of the three because a large push over a slow link is real work,
	// and it is still bounded because the relay's read loop answers nothing
	// else on that socket while a push runs.
	pushTimeout = 120 * time.Second
)

// maxStderrBytes bounds what a failure carries. git's stderr rides to the
// person inside a relay frame, and the hub drops a frame over MaxFrameBytes, so
// one command that wrote a megabyte of progress must not be able to take the
// answer with it. The tail is what is kept: git writes its progress first and
// its verdict last.
const maxStderrBytes = 4 << 10

// maxPathspecBytes bounds one command's pathspec arguments. A worktree with a
// hundred thousand dirty paths would otherwise build a command line past the
// operating system's limit and fail the commit for a reason that has nothing to
// do with git, so the pathspecs are chunked and the same command runs more than
// once. Staging in several passes is safe: each pass only adds to the index.
const maxPathspecBytes = 64 << 10

// Service serves the git channel for one worktree.
//
// It holds no handle and nothing to close: every operation is a git process,
// and the worktree itself is owned by the session that prepared it. The path is
// canonical and is proved to be the repository's top level at Open, which is
// the containment argument for a surface that stages "everything".
type Service struct {
	path string
	deny workspacesession.Denylist
}

// Open prepares a service for a worktree, and refuses one this package must not
// write to.
//
// It takes a context because it runs a command: the single local rev-parse that
// proves the worktree is a repository. The caller's context bounds it, so a
// session being torn down while it opens does not wait for a filesystem that
// has stopped answering.
func Open(ctx context.Context, worktree string, deny workspacesession.Denylist) (*Service, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, refuse(workspacesession.CodeUnsupported,
			"This runner has no git binary, so the git channel cannot be served",
			errors.Join(ErrNoGit, err))
	}
	canonical, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return nil, refuse(workspacesession.CodeUnsupported,
			"The worktree could not be resolved", errors.Join(ErrNotRepository, err))
	}
	service := &Service{path: canonical, deny: deny}
	toplevel := service.exec(ctx, readTimeout, nil, "rev-parse", "--show-toplevel")
	if toplevel.err != nil {
		return nil, refuse(workspacesession.CodeUnsupported,
			"This worktree is not a git repository", errors.Join(ErrNotRepository, toplevel.err))
	}
	top, err := filepath.EvalSymlinks(toplevel.stdout)
	if err != nil {
		return nil, refuse(workspacesession.CodeUnsupported,
			"The repository's top level could not be resolved", errors.Join(ErrNotRepository, err))
	}
	// The worktree has to be the repository's top level, not a directory inside
	// one. A commit from this package stages every dirty path, so a worktree
	// that was only a subdirectory of a larger repository would stage a
	// person's unrelated work elsewhere in that repository the moment they
	// asked for a commit here. There is no version of "commit everything" that
	// is correct when "everything" reaches past the worktree the person is
	// looking at, so the only safe answer is to refuse and serve the workspace
	// without the git capability.
	if filepath.Clean(top) != filepath.Clean(canonical) {
		return nil, refuse(workspacesession.CodeUnsupported,
			"This worktree is inside a git repository rather than being one",
			fmt.Errorf("%w: worktree %q is inside %q", ErrNotRepository, canonical, top))
	}
	return service, nil
}

// Status answers a status frame.
//
// Ahead and Behind are only filled in when the branch has a tracking ref. With
// no upstream they stay zero because they are unknown, not because the branch
// is in step, and the channel carries Upstream alongside them so a reader is
// never shown "0 ahead" for a branch nobody could compare (section 18.13).
func (s *Service) Status(ctx context.Context) (workspacesession.GitStatus, error) {
	branch, detached, err := s.head(ctx)
	if err != nil {
		return workspacesession.GitStatus{}, err
	}
	status := workspacesession.GitStatus{Branch: branch, Detached: detached}
	// An unborn branch has no commit to name, so a missing head sha is an
	// answer here rather than a failure.
	if head := s.exec(ctx, readTimeout, nil, "rev-parse", "HEAD"); head.err == nil {
		status.HeadSHA = head.stdout
	}
	if !detached {
		// A detached HEAD pushes nowhere, so it is reported with no remote
		// rather than with the one a branch would have used: naming a remote
		// the person cannot push to would enable a control that refuses.
		status.Remote = s.remote(ctx, branch)
		if upstream, ok := s.upstream(ctx); ok {
			status.Upstream = true
			status.Ahead, status.Behind = s.divergence(ctx, upstream)
		}
	}
	dirty, err := s.changes(ctx)
	if err != nil {
		return workspacesession.GitStatus{}, err
	}
	status.Dirty = len(dirty)
	return status, nil
}

// Commit stages every dirty path the denylist allows and commits them.
//
// The commit is the index, not a pathspec: staging and then committing plainly
// is what makes the exclusion real. Committing with --only and the allowed
// paths would re-add whatever was named, which for a rename means re-adding the
// side of it this package deliberately left out.
func (s *Service) Commit(ctx context.Context, message string, author, committer Identity) (workspacesession.GitCommitted, error) {
	if err := author.Validate(); err != nil {
		return workspacesession.GitCommitted{}, fail("The commit author is not a usable git identity", "", err)
	}
	if err := committer.Validate(); err != nil {
		return workspacesession.GitCommitted{}, fail("The commit committer is not a usable git identity", "", err)
	}
	dirty, err := s.changes(ctx)
	if err != nil {
		return workspacesession.GitCommitted{}, err
	}
	decided := s.partition(dirty)
	if decided.carried == 0 {
		if len(decided.excluded) > 0 {
			return workspacesession.GitCommitted{}, fail(
				"Every changed path in this worktree is refused by the project's secret patterns, so there is nothing to commit",
				"", ErrNothingToCommit)
		}
		return workspacesession.GitCommitted{}, fail(
			"The worktree is clean, so there is nothing to commit", "", ErrNothingToCommit)
	}
	if err := s.unstage(ctx, decided.indexed); err != nil {
		return workspacesession.GitCommitted{}, err
	}
	for _, chunk := range chunkPathspecs(decided.add) {
		add := s.exec(ctx, writeTimeout, nil, append([]string{"add", "-A", "--"}, chunk...)...)
		if add.err != nil {
			return workspacesession.GitCommitted{}, fail("The changes could not be staged for a commit", add.stderr, add.err)
		}
	}
	commit := s.exec(ctx, writeTimeout, author.commitEnvironment(committer),
		// The committer identity is passed as configuration as well as through
		// the environment, so a worktree with no configured git identity still
		// commits: git reads user.name and user.email when it builds the
		// committer line, and a runner-prepared checkout has neither.
		//
		// --no-gpg-sign is deliberate. The author is a person and the committer
		// is this machine, so a signature from whatever key the worktree's
		// configuration names would attest the machine's key over a person's
		// change; and a key with a passphrase has nobody here to type it.
		"-c", "user.name="+committer.Name, "-c", "user.email="+committer.Email,
		"commit", "--no-gpg-sign", "-m", message)
	if commit.err != nil {
		return workspacesession.GitCommitted{}, fail("The commit failed", commit.stderr, commit.err)
	}
	committed := workspacesession.GitCommitted{Files: decided.carried, Excluded: decided.excluded}
	if head := s.exec(ctx, readTimeout, nil, "rev-parse", "HEAD"); head.err == nil {
		committed.Commit = head.stdout
	}
	if branch, _, err := s.head(ctx); err == nil {
		committed.Branch = branch
	}
	return committed, nil
}

// Push sends the current branch to its remote.
func (s *Service) Push(ctx context.Context) (workspacesession.GitPushed, error) {
	branch, detached, err := s.head(ctx)
	if err != nil {
		return workspacesession.GitPushed{}, err
	}
	if detached {
		return workspacesession.GitPushed{}, fail(
			"HEAD is not on a branch, so there is nothing to push", "", ErrDetachedHead)
	}
	remote := s.remote(ctx, branch)
	if remote == "" {
		return workspacesession.GitPushed{}, fail(
			"This worktree has no remote to push to", "", ErrNoRemote)
	}
	// No force flag, ever: not --force, not --force-with-lease, not a leading
	// plus on the refspec. A push that would overwrite history on the remote is
	// refused by the remote, and the person is shown that refusal in git's own
	// words. Overwriting somebody else's commits is not a thing a header button
	// may do.
	push := s.exec(ctx, pushTimeout, nil, "push", remote, branch)
	if push.err != nil {
		return workspacesession.GitPushed{}, fail("The push failed", push.stderr, push.err)
	}
	pushed := workspacesession.GitPushed{Branch: branch, Remote: remote}
	if head := s.exec(ctx, readTimeout, nil, "rev-parse", "HEAD"); head.err == nil {
		pushed.Commit = head.stdout
	}
	return pushed, nil
}

// change is one record of git status --porcelain=v1 -z.
type change struct {
	// index and worktree are the X and Y status letters: what the index has
	// that HEAD does not, and what the worktree has that the index does not.
	index    byte
	worktree byte
	path     string
	// original is where a rename or a copy came from, empty otherwise. Both
	// sides are tested against the denylist and both are staged together,
	// because a rename is one change to two paths and a half-staged one records
	// a deletion nobody asked for.
	original string
}

// staged reports whether this path is already in the index. It matters for a
// denied path: the index belongs to the worktree rather than to this service,
// and whatever ran in it before could have added anything.
func (c change) staged() bool { return c.index != ' ' && c.index != '?' }

// paths lists every path this change touches, which is what the commit carries
// and what the denylist is tested against.
func (c change) paths() []string {
	if c.original == "" {
		return []string{c.path}
	}
	return []string{c.path, c.original}
}

// addPaths lists what git add has to be given for this change, which is not
// the same list.
//
// A record with a blank worktree column is already in the index: it needs no
// staging, and it must not be handed to git add at all, because the paths it
// names may no longer exist on either side. A staged rename leaves its original
// path in neither the worktree nor the index, a staged deletion leaves its path
// in neither, and a pathspec matching nothing fails the whole command -- which
// would make a worktree with one staged deletion in it impossible to commit
// from this surface.
func (c change) addPaths() []string {
	if c.worktree == ' ' {
		return nil
	}
	if renameOrCopy(c.worktree) && c.original != "" {
		return []string{c.path, c.original}
	}
	return []string{c.path}
}

// changes enumerates everything the worktree has that HEAD does not.
//
// This format is the one thing in this package that has to be parsed rather
// than asked for, so it is parsed properly: records are NUL-terminated, each is
// "XY <path>", and a rename or a copy is followed by a second NUL-terminated
// field holding the path it came from. -z is not a convenience -- it is what
// makes the parse total, because without it git quotes and escapes any path
// with a space or a newline in it, and this surface would then disagree with
// the worktree about what a file is called. --untracked-files=all is what makes
// an untracked directory arrive as its files, so the denylist decides per file
// rather than per directory.
func (s *Service) changes(ctx context.Context) ([]change, error) {
	status := s.exec(ctx, readTimeout, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if status.err != nil {
		return nil, fail("The worktree's status could not be read", status.stderr, status.err)
	}
	records := strings.Split(status.stdout, "\x00")
	changes := []change{}
	for index := 0; index < len(records); index++ {
		record := records[index]
		// "XY " and at least one character of path. The final NUL leaves an
		// empty record behind, which this also skips.
		if len(record) < 4 {
			continue
		}
		entry := change{index: record[0], worktree: record[1], path: record[3:]}
		if renameOrCopy(entry.index) || renameOrCopy(entry.worktree) {
			index++
			if index < len(records) {
				entry.original = records[index]
			}
		}
		changes = append(changes, entry)
	}
	return changes, nil
}

// renameOrCopy reports whether a status letter means a second path field
// follows.
func renameOrCopy(letter byte) bool { return letter == 'R' || letter == 'C' }

// plan is what one commit will do.
type plan struct {
	// add is the pathspecs to stage, and carried is how many paths the commit
	// will hold. They are two numbers rather than one because a change already
	// in the index is committed without being staged again.
	add     []string
	carried int
	// excluded names the paths the denylist kept out. It is sorted and
	// de-duplicated because a person reads it: a rename names both of its
	// paths, and a path named twice reads like two files.
	excluded []string
	// indexed is the denied paths that are already staged and have to be taken
	// back out of the index before the commit records it.
	indexed []string
}

// partition decides, for every dirty path, whether the commit carries it.
func (s *Service) partition(dirty []change) plan {
	decided := plan{add: []string{}, excluded: []string{}, indexed: []string{}}
	for _, entry := range dirty {
		if s.deny.DeniedDiffPath(entry.path, entry.original) {
			decided.excluded = append(decided.excluded, entry.paths()...)
			if entry.staged() {
				decided.indexed = append(decided.indexed, entry.paths()...)
			}
			continue
		}
		decided.add = append(decided.add, entry.addPaths()...)
		decided.carried += len(entry.paths())
	}
	slices.Sort(decided.excluded)
	decided.excluded = slices.Compact(decided.excluded)
	return decided
}

// unstage takes a denied path back out of the index before the commit.
//
// A denied path is never staged by this package, but it may already be staged,
// and a plain commit records the index. Leaving one in would put exactly the
// content section 18.4 refuses to show into a commit, and from there onto a
// remote, which is the one outcome this whole filter exists to prevent.
// Restoring the entry from HEAD leaves the file in the worktree untouched and
// still dirty, which is the state the person is then shown.
func (s *Service) unstage(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	// On an unborn branch there is no HEAD to restore an index entry from, and
	// every entry in the index is an addition there, so dropping it from the
	// index is the same operation. --ignore-unmatch keeps a path git has
	// already forgotten from failing the commit.
	reset := []string{"reset", "--quiet", "--"}
	if !s.hasHead(ctx) {
		reset = []string{"rm", "--cached", "--quiet", "--ignore-unmatch", "--"}
	}
	for _, chunk := range chunkPathspecs(paths) {
		outcome := s.exec(ctx, writeTimeout, nil, append(slices.Clone(reset), chunk...)...)
		if outcome.err != nil {
			return fail("A path the project's secret patterns refuse could not be taken out of the index, so the commit was not made",
				outcome.stderr, outcome.err)
		}
	}
	return nil
}

// head reports the branch HEAD is on. "HEAD" is git's own answer for a detached
// HEAD, which this channel reports as a flag rather than as a branch of that
// name.
func (s *Service) head(ctx context.Context) (branch string, detached bool, err error) {
	current := s.exec(ctx, readTimeout, nil, "rev-parse", "--abbrev-ref", "HEAD")
	if current.err == nil {
		return current.stdout, current.stdout == "HEAD", nil
	}
	// A repository with no commits at all is a real state for a workspace -- a
	// fresh checkout of an empty repository -- and rev-parse refuses HEAD there
	// because there is no commit to resolve. symbolic-ref still names the branch
	// the first commit will land on, which is what the person is shown.
	unborn := s.exec(ctx, readTimeout, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if unborn.err == nil && unborn.stdout != "" {
		return unborn.stdout, false, nil
	}
	return "", false, fail("The worktree's branch could not be read", current.stderr, current.err)
}

// hasHead reports whether the branch has a commit yet.
func (s *Service) hasHead(ctx context.Context) bool {
	return s.exec(ctx, readTimeout, nil, "rev-parse", "--verify", "--quiet", "HEAD").err == nil
}

// upstream names the branch's tracking ref, when it has one.
func (s *Service) upstream(ctx context.Context) (string, bool) {
	tracking := s.exec(ctx, readTimeout, nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if tracking.err != nil || tracking.stdout == "" {
		return "", false
	}
	return tracking.stdout, true
}

// divergence reports how far HEAD is from its upstream.
//
// git rev-list --left-right --count <upstream>...HEAD prints two counts: the
// left side is what the upstream has and HEAD does not, which is behind, and
// the right side is what HEAD has and the upstream does not, which is ahead.
// Reading them the other way round would tell a person to pull when they should
// push.
func (s *Service) divergence(ctx context.Context, upstream string) (ahead, behind int) {
	counts := s.exec(ctx, readTimeout, nil, "rev-list", "--left-right", "--count", upstream+"...HEAD")
	if counts.err != nil {
		return 0, 0
	}
	fields := strings.Fields(counts.stdout)
	if len(fields) != 2 {
		return 0, 0
	}
	// A count that does not parse is reported as no divergence at all rather
	// than as half of one: "3 ahead, 0 behind" read off a line git wrote in a
	// format this does not recognise would be a confident lie.
	left, leftErr := strconv.Atoi(fields[0])
	right, rightErr := strconv.Atoi(fields[1])
	if leftErr != nil || rightErr != nil {
		return 0, 0
	}
	return right, left
}

// remote resolves what a push from this branch targets, in the order section
// 18.13 gives: the branch's own configured remote, then origin, then the only
// remote there is. The order matters -- a branch configured to push somewhere
// must never be pushed to origin merely because origin also exists -- and so
// does the last case ending in nothing: with two remotes and no reason to
// prefer either, guessing would push a person's branch somewhere they never
// named.
func (s *Service) remote(ctx context.Context, branch string) string {
	configured := s.exec(ctx, readTimeout, nil, "config", "--get", "branch."+branch+".remote")
	if configured.err == nil && configured.stdout != "" {
		return configured.stdout
	}
	listed := s.exec(ctx, readTimeout, nil, "remote")
	if listed.err != nil {
		return ""
	}
	names := []string{}
	for _, name := range strings.Split(listed.stdout, "\n") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	if slices.Contains(names, "origin") {
		return "origin"
	}
	if len(names) == 1 {
		return names[0]
	}
	return ""
}

// Identity is who a commit is authored and committed as.
type Identity struct {
	Name  string
	Email string
}

// Validate refuses an identity git could not use, and one that could forge a
// second identity into a commit.
//
// git records authorship as "Name <email> timestamp" on a single line. A name
// or an address carrying "<", ">", a newline or a NUL byte could close that
// line and open another, which would let whoever supplied it attribute a commit
// to somebody who never made one -- the exact thing the hub stamping the actor
// is there to prevent. Both halves must be present because git refuses an empty
// ident itself, and its advice for one is to configure the machine's identity,
// which is how a person's change ends up attributed to the runner account.
func (i Identity) Validate() error {
	halves := []struct {
		label string
		value string
	}{{"name", i.Name}, {"email", i.Email}}
	for _, half := range halves {
		if strings.TrimSpace(half.value) == "" {
			return fmt.Errorf("%w: the %s is empty", ErrInvalidIdentity, half.label)
		}
		if strings.ContainsAny(half.value, "<>\n\r\x00") {
			return fmt.Errorf("%w: the %s carries a character git's author line cannot", ErrInvalidIdentity, half.label)
		}
	}
	return nil
}

// commitEnvironment names the author and the committer for one commit.
//
// Both are given explicitly rather than left to git's own defaults, because
// git's default committer is whatever the machine is configured as: a commit
// that named only an author would record the runner account as having made the
// person's change, or fail with advice about configuring an identity.
func (i Identity) commitEnvironment(committer Identity) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + i.Name,
		"GIT_AUTHOR_EMAIL=" + i.Email,
		"GIT_COMMITTER_NAME=" + committer.Name,
		"GIT_COMMITTER_EMAIL=" + committer.Email,
	}
}

// result is what one git command produced.
type result struct {
	stdout string
	stderr string
	err    error
}

// exec runs one git command in the worktree.
//
// It returns no error of its own, because for several of these commands a
// non-zero exit is an answer rather than a failure: "this branch has no
// configured remote" and "this branch has no upstream" are both exit 1, and a
// caller that could not see the difference would report a broken worktree for
// an ordinary one.
func (s *Service) exec(ctx context.Context, timeout time.Duration, extraEnv []string, args ...string) result {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, "git", args...)
	// The worktree is the working directory rather than a -C argument, for the
	// same reason the files service does it: every argument then being a
	// literal is what makes it plain, here and to a scanner, that nothing a
	// person sent can reach git as an option.
	command.Dir = s.path
	command.Env = append(s.environment(), extraEnv...)
	// No standard input at all: a git that decided to ask for something gets
	// end-of-file and fails instead of waiting.
	command.Stdin = nil
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return result{
		stdout: strings.TrimRight(stdout.String(), "\n"),
		stderr: tail(stderr.String()),
		err:    err,
	}
}

// environment is what every git command runs in.
//
// The runner's own environment is kept rather than stripped: HOME is where git
// finds its configuration and its credential helper, and SSH_AUTH_SOCK is how a
// push authenticates at all. What is overridden are the two variables that make
// a prompt possible. GIT_TERMINAL_PROMPT=0 turns off the terminal prompt git
// raises for a password, and an empty GIT_ASKPASS leaves no helper to ask
// through, so a credential the runner does not already hold fails the command
// instead of waiting for a person who is not there.
func (s *Service) environment() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
}

// tail keeps the last maxStderrBytes of what git wrote, marking the cut so a
// reader knows the beginning is missing.
func tail(value string) string {
	trimmed := strings.TrimRight(value, "\n")
	if len(trimmed) <= maxStderrBytes {
		return trimmed
	}
	return "[earlier output omitted]\n" + trimmed[len(trimmed)-maxStderrBytes:]
}

// pathspec makes one literal pathspec.
//
// Without :(literal) git reads a pathspec as a glob with magic: a file actually
// named "a*" would stage every path beginning with "a", and one named
// ":(exclude)secret" would turn into an exclusion. Both are legal file names on
// every platform the runner runs on, and neither may be allowed to decide what
// a commit contains.
func pathspec(relative string) string { return ":(literal)" + relative }

// chunkPathspecs splits paths into command lines no longer than
// maxPathspecBytes.
func chunkPathspecs(paths []string) [][]string {
	chunks := [][]string{}
	current := []string{}
	size := 0
	for _, relative := range paths {
		spec := pathspec(relative)
		if len(current) > 0 && size+len(spec)+1 > maxPathspecBytes {
			chunks = append(chunks, current)
			current, size = []string{}, 0
		}
		current = append(current, spec)
		size += len(spec) + 1
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
