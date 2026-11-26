package checkpoint

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	return ScreenCredentials(blob)
}

func ScreenCredentials(blob string) error {
	for {
		match := secretPattern.FindStringIndex(blob)
		if match == nil {
			return nil
		}
		if !syntheticRedactionFixture(blob[match[0]:match[1]]) {
			return fmt.Errorf("%w: possible credential in selected content", ErrUnsafe)
		}
		blob = blob[match[1]:]
	}
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

const CheckpointRefPrefix = tracker.CheckpointRefPrefix

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
