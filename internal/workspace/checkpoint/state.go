package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
	"github.com/digitaldrywood/detent/internal/workspace/internal/model"
)

var ErrUnsafe = errors.New("checkpoint could not be safely published")

var secretPattern = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[A-Z0-9]{16}|sk-proj-[A-Za-z0-9_-]{30,})`)

type Selection struct {
	HeadSHA  string   `json:"head_sha"`
	Paths    []string `json:"paths"`
	Reviewed bool     `json:"reviewed"`
}

type Plan struct {
	Info      model.Info `json:"workspace"`
	BaseSHA   string     `json:"base_sha"`
	RemoteURL string     `json:"-"`
	Journal   string     `json:"-"`
}

type Record struct {
	Schema        int      `json:"schema"`
	Reason        string   `json:"reason"`
	Status        string   `json:"status"`
	WorkspacePath string   `json:"workspace_path"`
	Branch        string   `json:"branch"`
	HeadSHA       string   `json:"head_sha,omitempty"`
	Paths         []string `json:"paths,omitempty"`
	Published     bool     `json:"published"`
	PullRequest   string   `json:"pull_request,omitempty"`
	Detail        string   `json:"detail"`
}

func WriteRecord(plan Plan, record Record) (returnErr error) {
	if plan.Journal == "" {
		return errors.New("checkpoint journal is unavailable")
	}
	record.Schema = 1
	record.WorkspacePath = plan.Info.Path
	record.Branch = plan.Info.PublishBranch()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(plan.Journal), "detent-checkpoint-*.json")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.Remove(file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	if err := errors.Join(writeErr, syncErr, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), plan.Journal)
}

type GitRunner func(context.Context, ...string) (string, error)

func Environment(environment procgroup.Environment) []string {
	env := []string{"GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0"}
	for key, value := range environment.Variables {
		env = append(env, key+"="+value)
	}
	return env
}

func Git(path string, environment procgroup.Environment) GitRunner {
	env := Environment(environment)
	return func(ctx context.Context, args ...string) (string, error) {
		return gitexec.WithEnv(ctx, path, env, args...)
	}
}

func Guard(ctx context.Context, git GitRunner, plan Plan, head string, validate func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validate(ctx); err != nil {
		return err
	}
	branch, err := git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) != plan.Info.Branch {
		return errors.Join(fmt.Errorf("%w: owned branch changed", ErrUnsafe), err)
	}
	current, err := git(ctx, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(current) != head {
		return errors.Join(fmt.Errorf("%w: reviewed head changed", ErrUnsafe), err)
	}
	return nil
}

func RemoteHead(ctx context.Context, git GitRunner, plan Plan) (string, error) {
	output, err := git(ctx, "ls-remote", "--heads", plan.RemoteURL, "refs/heads/"+plan.Info.PublishBranch())
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || fields[1] != "refs/heads/"+plan.Info.PublishBranch() {
		return "", fmt.Errorf("%w: unexpected remote ref", ErrUnsafe)
	}
	return fields[0], nil
}

func History(ctx context.Context, git GitRunner, base, head string, paths []string) error {
	if _, err := git(ctx, "merge-base", "--is-ancestor", base, head); err != nil {
		return fmt.Errorf("%w: checkpoint base changed", ErrUnsafe)
	}
	commits, err := git(ctx, "rev-list", base+".."+head)
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(commits) {
		changed, err := git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return err
		}
		for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
			if path == "" {
				continue
			}
			if !PathAllowed(path) || !slices.Contains(paths, path) {
				return fmt.Errorf("%w: commit contains an unapproved path %q", ErrUnsafe, path)
			}
			if err := Blob(ctx, git, commit+":", path); err != nil {
				return err
			}
		}
	}
	return nil
}

func Blob(ctx context.Context, git GitRunner, prefix, path string) error {
	args := []string{"ls-tree", "-z", strings.TrimSuffix(prefix, ":"), "--", path}
	if prefix == ":" {
		args = []string{"ls-files", "--stage", "-z", "--", path}
	}
	entry, err := git(ctx, args...)
	if err != nil {
		return err
	}
	if entry == "" {
		return nil
	}
	header, name, ok := strings.Cut(strings.TrimSuffix(entry, "\x00"), "\t")
	fields := strings.Fields(header)
	if !ok || name != path || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") {
		return fmt.Errorf("%w: selected history must contain regular files", ErrUnsafe)
	}
	blob, err := git(ctx, "show", prefix+path)
	if err != nil {
		return err
	}
	for _, match := range secretPattern.FindAllString(blob, -1) {
		if !syntheticRedactionFixture(match) {
			return fmt.Errorf("%w: possible credential in selected content", ErrUnsafe)
		}
	}
	return nil
}

func syntheticRedactionFixture(match string) bool {
	switch fmt.Sprintf("%x", sha256.Sum256([]byte(match))) {
	case "76f476d0bf47418551fdd8ae355d42706b642301bb4172204a1ad4de12c4b840",
		"c5988f95044fbaa2bd149bfc96340c1bacb8b6095d846468348b0a081594162c",
		"743554670c6065b3f7f13ac4f07e392f977b3556ceb7457411633c454bcbece8":
		return true
	default:
		return false
	}
}

func PathAllowed(path string) bool {
	if path == "" || !filepath.IsLocal(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsAny(path, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(strings.ToLower(path), "/") {
		if strings.HasPrefix(part, ".env") && !strings.HasSuffix(part, ".example") {
			return false
		}
		if part == ".git" || part == ".ssh" || part == "node_modules" || part == "tmp" || part == "secrets" || part == "credentials.json" || strings.HasPrefix(part, "id_rsa") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".p12") {
			return false
		}
	}
	return true
}

const (
	CheckpointRefPrefix   = tracker.CheckpointRefPrefix
	MaxCheckpointRefBytes = 20 << 20
)

type Ref struct {
	Ref         string
	CommitSHA   string
	HeadSHA     string
	BaseSHA     string
	TreeSHA     string
	Transaction *Transaction `json:"-"`
}

func RefGit(path string, env ...string) GitRunner {
	env = append([]string{
		"GIT_LITERAL_PATHSPECS=1", "GIT_TERMINAL_PROMPT=0",
	}, env...)
	return func(ctx context.Context, args ...string) (string, error) {
		return gitexec.WithEnv(ctx, path, env, args...)
	}
}

// Snapshot writes HEAD plus every uncommitted, non-ignored change
// into the temporary index, leaving excluded paths at their HEAD version.
func Snapshot(ctx context.Context, git, index GitRunner) (head, tree string, included []string, err error) {
	output, err := git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", nil, fmt.Errorf("read checkpoint head: %w", err)
	}
	head = strings.TrimSpace(output)
	if _, err := index(ctx, "read-tree", "HEAD"); err != nil {
		return "", "", nil, fmt.Errorf("seed checkpoint index: %w", err)
	}
	if _, err := index(ctx, "add", "-A", "--", "."); err != nil {
		return "", "", nil, fmt.Errorf("stage checkpoint content: %w", err)
	}
	changed, err := index(ctx, "diff", "--cached", "--name-only", "--no-renames", "-z", "HEAD", "--")
	if err != nil {
		return "", "", nil, err
	}
	var excluded []string
	for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
		switch {
		case path == "":
		case !PathAllowed(path) || slices.ContainsFunc(gitexec.HandoffDiffExcludes, func(exclude string) bool { return strings.HasPrefix(path, exclude) }):
			excluded = append(excluded, path)
		default:
			included = append(included, path)
		}
	}
	if len(excluded) != 0 {
		if _, err := index(ctx, append([]string{"reset", "-q", "HEAD", "--"}, excluded...)...); err != nil {
			return "", "", nil, fmt.Errorf("exclude checkpoint paths: %w", err)
		}
	}
	output, err = index(ctx, "write-tree")
	if err != nil {
		return "", "", nil, fmt.Errorf("write checkpoint tree: %w", err)
	}
	return head, strings.TrimSpace(output), included, nil
}

// Matches reports whether an existing worktree already holds the
// checkpoint, so restoring it would only quarantine an identical copy.
func Matches(ctx context.Context, path string, checkpoint *Ref) (matched bool) {
	if checkpoint.Transaction != nil {
		return TransactionMatches(ctx, path, checkpoint.Transaction)
	}
	scratch, err := gitexec.Scratch()
	if err != nil {
		return false
	}
	defer func() { matched = os.RemoveAll(scratch) == nil && matched }()
	head, tree, _, err := Snapshot(ctx, RefGit(path), RefGit(path, "GIT_INDEX_FILE="+filepath.Join(scratch, "index")))
	return err == nil && head == checkpoint.HeadSHA && tree == checkpoint.TreeSHA
}

func UnpublishedHistory(ctx context.Context, git GitRunner, head string) error {
	commits, err := git(ctx, "rev-list", head, "--not", "--remotes")
	if err != nil {
		return err
	}
	for _, commit := range strings.Fields(commits) {
		changed, err := git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-m", "-z", commit)
		if err != nil {
			return err
		}
		for _, path := range strings.Split(strings.TrimSuffix(changed, "\x00"), "\x00") {
			if path == "" {
				continue
			}
			if !PathAllowed(path) {
				return fmt.Errorf("%w: unpublished commit contains excluded path %q", ErrUnsafe, path)
			}
			if err := Blob(ctx, git, commit+":", path); err != nil {
				return err
			}
		}
	}
	return nil
}

func RestoreTree(ctx context.Context, path string, checkpoint *Ref) error {
	if checkpoint.Transaction != nil {
		return RestoreTransaction(ctx, path, checkpoint)
	}
	if _, err := gitexec.Run(ctx, path, "-c", "core.hooksPath="+os.DevNull, "read-tree", "-u", "--reset", checkpoint.TreeSHA); err != nil {
		return fmt.Errorf("restore checkpoint tree: %w", err)
	}
	if _, err := gitexec.Run(ctx, path, "reset", "-q", "HEAD"); err != nil {
		return fmt.Errorf("unstage restored checkpoint: %w", err)
	}
	return nil
}

type IndexEntry struct {
	Path  string
	Mode  string
	Blob  string
	Stage int
}

type Transaction struct {
	DropRedundant bool
	Schema        int
	Branch        string
	Head          string
	Original      string
	Onto          string
	Stopped       string
	Worktree      string
	Index         []IndexEntry
	Done          []string
	Todo          []string
	Rewritten     [][2]string
}

func Replay(ctx context.Context, git GitRunner, data string) ([]string, error) {
	var commits []string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "pick" && fields[0] != "p" {
			return nil, fmt.Errorf("%w: unsupported rebase operation", ErrUnsafe)
		}
		output, err := git(ctx, "rev-parse", "--verify", fields[1]+"^{commit}")
		if err != nil || !gitexec.ValidHead(strings.TrimSpace(output)) {
			return nil, errors.Join(ErrUnsafe, err)
		}
		commits = append(commits, strings.TrimSpace(output))
	}
	return commits, nil
}

func CaptureTransaction(ctx context.Context, path, branch, scratch string) (*Transaction, error) {
	git := RefGit(path)
	dir, err := gitexec.PathFor(ctx, path, "rebase-merge")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: only merge-backend source replay is portable", ErrUnsafe)
	}
	allowed := []string{"head-name", "orig-head", "onto", "stopped-sha", "done", "git-rebase-todo", "git-rebase-todo.backup", "rewritten-list", "msgnum", "end", "message", "author-script", "patch", "interactive", "no-reschedule-failed-exec", "gpg_sign_opt", "drop_redundant_commits"}
	for _, entry := range entries {
		if !slices.Contains(allowed, entry.Name()) || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%w: unsupported rebase metadata %q", ErrUnsafe, entry.Name())
		}
	}
	read := func(name string) (string, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(data)), err
	}
	t := &Transaction{Schema: 1, Branch: branch}
	if _, err := read("drop_redundant_commits"); err == nil {
		t.DropRedundant = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	name, err := read("head-name")
	if err != nil || name != "refs/heads/"+branch {
		return nil, errors.Join(ErrUnsafe, err)
	}
	for name, field := range map[string]*string{"orig-head": &t.Original, "onto": &t.Onto, "stopped-sha": &t.Stopped} {
		*field, err = read(name)
		if err != nil || !gitexec.ValidHead(*field) {
			return nil, errors.Join(ErrUnsafe, err)
		}
	}
	for name, field := range map[string]*[]string{"done": &t.Done, "git-rebase-todo": &t.Todo} {
		data, err := read(name)
		if err != nil {
			return nil, err
		}
		*field, err = Replay(ctx, git, data)
		if err != nil {
			return nil, err
		}
	}
	if len(t.Done) == 0 || t.Done[len(t.Done)-1] != t.Stopped {
		return nil, fmt.Errorf("%w: stopped source differs from replay", ErrUnsafe)
	}
	for name, want := range map[string]int{"msgnum": len(t.Done), "end": len(t.Done) + len(t.Todo)} {
		value, err := read(name)
		if err != nil || value != strconv.Itoa(want) {
			return nil, errors.Join(ErrUnsafe, err)
		}
	}
	data, err := read("rewritten-list")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !gitexec.ValidHead(fields[0]) || !gitexec.ValidHead(fields[1]) {
			return nil, ErrUnsafe
		}
		t.Rewritten = append(t.Rewritten, [2]string{fields[0], fields[1]})
	}
	expectedAuthor, err := AuthorScript(ctx, git, t.Stopped)
	if err != nil {
		return nil, err
	}
	author, err := read("author-script")
	if err != nil || author != strings.TrimSpace(expectedAuthor) {
		return nil, errors.Join(fmt.Errorf("%w: altered author metadata", ErrUnsafe), err)
	}
	expectedMessage, err := git(ctx, "show", "-s", "--format=%B", t.Stopped)
	if err != nil {
		return nil, err
	}
	message, err := read("message")
	if body, conflicts, ok := strings.Cut(message, "\n\n# Conflicts:\n"); ok {
		for _, line := range strings.Split(conflicts, "\n") {
			if !strings.HasPrefix(line, "#\t") || !PathAllowed(strings.TrimPrefix(line, "#\t")) {
				return nil, ErrUnsafe
			}
		}
		message = body
	}
	if err != nil || message != strings.TrimSpace(expectedMessage) {
		return nil, errors.Join(fmt.Errorf("%w: altered message metadata", ErrUnsafe), err)
	}
	index := RefGit(path, "GIT_INDEX_FILE="+filepath.Join(scratch, "worktree-index"))
	t.Head, t.Worktree, _, err = Snapshot(ctx, git, index)
	if err != nil {
		return nil, err
	}
	output, err := git(ctx, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	for _, entry := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, ErrUnsafe
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		t.Index = append(t.Index, IndexEntry{Path: path, Mode: fields[0], Blob: fields[1], Stage: stage})
	}
	return t, nil
}

func ValidateTransaction(ctx context.Context, git GitRunner, t *Transaction) error {
	if t.Schema != 1 || t.Branch == "" || strings.ContainsAny(t.Branch, "\x00\r\n") || !gitexec.ValidHead(t.Worktree) || len(t.Done) == 0 || t.Done[len(t.Done)-1] != t.Stopped {
		return fmt.Errorf("%w: incompatible checkpoint transaction", ErrUnsafe)
	}
	if _, err := git(ctx, "check-ref-format", "refs/heads/"+t.Branch); err != nil {
		return errors.Join(ErrUnsafe, err)
	}
	heads := append([]string{t.Head, t.Original, t.Onto, t.Stopped}, t.Done...)
	heads = append(heads, t.Todo...)
	for _, pair := range t.Rewritten {
		heads = append(heads, pair[0], pair[1])
	}
	for _, head := range heads {
		if !gitexec.ValidHead(head) {
			return ErrUnsafe
		}
		if _, err := git(ctx, "cat-file", "-e", head+"^{commit}"); err != nil {
			return errors.Join(ErrUnsafe, err)
		}
		if err := UnpublishedHistory(ctx, git, head); err != nil {
			return err
		}
	}
	for _, source := range append(slices.Clone(t.Done), t.Todo...) {
		if _, err := git(ctx, "merge-base", "--is-ancestor", source, t.Original); err != nil {
			return errors.Join(ErrUnsafe, err)
		}
	}
	if _, err := git(ctx, "merge-base", "--is-ancestor", t.Onto, t.Head); err != nil {
		return errors.Join(ErrUnsafe, err)
	}
	for _, pair := range t.Rewritten {
		if !slices.Contains(t.Done, pair[0]) || pair[0] == t.Stopped {
			return ErrUnsafe
		}
		if _, err := git(ctx, "merge-base", "--is-ancestor", pair[1], t.Head); err != nil {
			return errors.Join(ErrUnsafe, err)
		}
	}
	baseline, err := git(ctx, "ls-tree", "-r", "-z", t.Head)
	if err != nil {
		return err
	}
	unchanged := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSuffix(baseline, "\x00"), "\x00") {
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if ok && len(fields) == 3 {
			unchanged[path] = fields[0] + " " + fields[2]
		}
	}
	seen := make(map[string]map[int]bool)
	for _, entry := range t.Index {
		if entry.Stage < 0 || entry.Stage > 3 || entry.Mode != "100644" && entry.Mode != "100755" || !gitexec.ValidHead(entry.Blob) {
			return fmt.Errorf("%w: unsafe index alternative", ErrUnsafe)
		}
		stages := seen[entry.Path]
		if stages == nil {
			stages = make(map[int]bool)
			seen[entry.Path] = stages
		}
		if stages[entry.Stage] || entry.Stage == 0 && len(stages) > 0 || entry.Stage > 0 && stages[0] {
			return ErrUnsafe
		}
		stages[entry.Stage] = true
		if entry.Stage != 0 || unchanged[entry.Path] != entry.Mode+" "+entry.Blob {
			if !PathAllowed(entry.Path) || strings.HasPrefix(entry.Path, ".detent/") {
				return fmt.Errorf("%w: unsafe index alternative", ErrUnsafe)
			}
			if err := Alternative(ctx, git, entry.Blob); err != nil {
				return err
			}
		}
	}
	output, err := git(ctx, "ls-tree", "-r", "-z", t.Worktree)
	if err != nil {
		return err
	}
	worktree := make(map[string]string)
	for _, entry := range strings.Split(strings.TrimSuffix(output, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		header, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || fields[0] != "100644" && fields[0] != "100755" || fields[1] != "blob" {
			return ErrUnsafe
		}
		worktree[path] = fields[0] + " " + fields[2]
		if unchanged[path] != fields[0]+" "+fields[2] {
			if !PathAllowed(path) || strings.HasPrefix(path, ".detent/") {
				return ErrUnsafe
			}
			if err := Alternative(ctx, git, fields[2]); err != nil {
				return err
			}
		}
	}
	for path, baseline := range unchanged {
		if (!PathAllowed(path) || strings.HasPrefix(path, ".detent/")) && (!seen[path][0] || worktree[path] != baseline) {
			return fmt.Errorf("%w: excluded baseline path changed", ErrUnsafe)
		}
	}
	return nil
}

func Alternative(ctx context.Context, git GitRunner, blob string) error {
	kind, err := git(ctx, "cat-file", "-t", blob)
	if err != nil || strings.TrimSpace(kind) != "blob" {
		return errors.Join(ErrUnsafe, err)
	}
	size, err := git(ctx, "cat-file", "-s", blob)
	if err != nil {
		return err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	if err != nil || bytes > MaxCheckpointRefBytes {
		return errors.Join(ErrUnsafe, err)
	}
	data, err := git(ctx, "cat-file", "blob", blob)
	if err != nil {
		return err
	}
	for _, match := range secretPattern.FindAllString(data, -1) {
		if !syntheticRedactionFixture(match) {
			return fmt.Errorf("%w: possible credential in content alternative", ErrUnsafe)
		}
	}
	return nil
}

func ReadTransaction(ctx context.Context, git GitRunner, checkpoint *Ref) (*Transaction, error) {
	digest, err := git(ctx, "rev-parse", checkpoint.CommitSHA+":transaction.json")
	if err != nil || strings.TrimSpace(digest) != checkpoint.TreeSHA {
		return nil, errors.Join(ErrUnsafe, err)
	}
	data, err := git(ctx, "cat-file", "blob", checkpoint.TreeSHA)
	if err != nil || len(data) > MaxCheckpointRefBytes {
		return nil, errors.Join(ErrUnsafe, err)
	}
	var transaction Transaction
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&transaction); err != nil {
		return nil, errors.Join(ErrUnsafe, err)
	}
	if err := ValidateTransaction(ctx, git, &transaction); err != nil {
		return nil, err
	}
	worktree, err := git(ctx, "rev-parse", checkpoint.CommitSHA+":worktree")
	if err != nil || strings.TrimSpace(worktree) != transaction.Worktree {
		return nil, errors.Join(ErrUnsafe, err)
	}
	return &transaction, nil
}

func TransactionMatches(ctx context.Context, path string, transaction *Transaction) (matched bool) {
	scratch, err := gitexec.Scratch()
	if err != nil {
		return false
	}
	defer func() { matched = os.RemoveAll(scratch) == nil && matched }()
	actual, err := CaptureTransaction(ctx, path, transaction.Branch, scratch)
	return err == nil && reflect.DeepEqual(actual, transaction)
}

func RestoreTransaction(ctx context.Context, path string, checkpoint *Ref) error {
	t := checkpoint.Transaction
	git := RefGit(path)
	if _, err := git(ctx, "-c", "core.hooksPath="+os.DevNull, "read-tree", "-u", "--reset", t.Worktree); err != nil {
		return err
	}
	if _, err := git(ctx, "read-tree", "--empty"); err != nil {
		return err
	}
	var index strings.Builder
	for _, entry := range t.Index {
		fmt.Fprintf(&index, "%s %s %d\t%s\x00", entry.Mode, entry.Blob, entry.Stage, entry.Path)
	}
	if _, err := gitexec.WithInput(ctx, path, []string{"GIT_LITERAL_PATHSPECS=1"}, false, index.String(), "update-index", "-z", "--index-info"); err != nil {
		return err
	}
	if _, err := git(ctx, "update-ref", "--no-deref", "HEAD", t.Head); err != nil {
		return err
	}
	if _, err := git(ctx, "update-ref", "refs/heads/"+t.Branch, t.Original, t.Head); err != nil {
		return err
	}
	dir, err := gitexec.PathFor(ctx, path, "rebase-merge")
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	picks := func(commits []string) string {
		var text strings.Builder
		for _, commit := range commits {
			fmt.Fprintf(&text, "pick %s\n", commit)
		}
		return text.String()
	}
	message, err := git(ctx, "show", "-s", "--format=%B", t.Stopped)
	if err != nil {
		return err
	}
	script, err := AuthorScript(ctx, git, t.Stopped)
	if err != nil {
		return err
	}
	files := map[string]string{"head-name": "refs/heads/" + t.Branch + "\n", "orig-head": t.Original + "\n", "onto": t.Onto + "\n", "stopped-sha": t.Stopped + "\n", "done": picks(t.Done), "git-rebase-todo": picks(t.Todo), "msgnum": strconv.Itoa(len(t.Done)) + "\n", "end": strconv.Itoa(len(t.Done)+len(t.Todo)) + "\n", "message": message, "author-script": script, "interactive": "", "no-reschedule-failed-exec": ""}
	if t.DropRedundant {
		files["drop_redundant_commits"] = ""
	}
	var rewritten strings.Builder
	for _, pair := range t.Rewritten {
		fmt.Fprintf(&rewritten, "%s %s\n", pair[0], pair[1])
	}
	if rewritten.Len() > 0 {
		files["rewritten-list"] = rewritten.String()
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			return err
		}
	}
	if _, err := git(ctx, "update-ref", "REBASE_HEAD", t.Stopped); err != nil {
		return err
	}
	if !TransactionMatches(ctx, path, t) {
		return fmt.Errorf("%w: restored transaction differs from checkpoint", ErrUnsafe)
	}
	return nil
}

func AuthorScript(ctx context.Context, git GitRunner, stopped string) (string, error) {
	author, err := git(ctx, "show", "-s", "--date=raw", "--format=%an%x00%ae%x00%ad", stopped)
	if err != nil {
		return "", err
	}
	fields := strings.Split(strings.TrimSuffix(author, "\n"), "\x00")
	if len(fields) != 3 {
		return "", ErrUnsafe
	}
	fields[2] = "@" + fields[2]
	var script strings.Builder
	for i, name := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE"} {
		fmt.Fprintf(&script, "%s='%s'\n", name, strings.ReplaceAll(fields[i], "'", "'\\''"))
	}
	return script.String(), nil
}
